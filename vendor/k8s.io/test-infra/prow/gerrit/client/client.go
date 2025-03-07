/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package client implements a client that can handle multiple gerrit instances
// derived from https://github.com/andygrunwald/go-gerrit
package client

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	gerrit "github.com/andygrunwald/go-gerrit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/version"
)

const (
	// CodeReview is the default (soon to be removed) gerrit code review label
	CodeReview = "Code-Review"

	// Merged status indicates a Gerrit change has been merged
	Merged = "MERGED"
	// New status indicates a Gerrit change is new (ie pending)
	New = "NEW"

	// ReadyForReviewMessage are the messages for a Gerrit change if it's changed
	// from Draft to Active.
	// This message will be sent if users press the `MARK AS ACTIVE` button.
	ReadyForReviewMessageFixed = "Set Ready For Review"
	// This message will be sent if users press the `SEND AND START REVIEW` button.
	ReadyForReviewMessageCustomizable = "This change is ready for review."

	ResultError   = "ERROR"
	ResultSuccess = "SUCCESS"
)

var clientMetrics = struct {
	queryResults *prometheus.CounterVec
}{
	queryResults: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gerrit_query_results",
		Help: "Count of Gerrit API queries by instance, repo, and result.",
	}, []string{
		"org",
		"repo",
		"result",
	}),
}

func init() {
	prometheus.MustRegister(clientMetrics.queryResults)
}

// ProjectsFlag is the flag type for gerrit projects when initializing a gerrit client
type ProjectsFlag map[string][]string

func (p ProjectsFlag) String() string {
	var hosts []string
	for host, repos := range p {
		hosts = append(hosts, host+"="+strings.Join(repos, ","))
	}
	return strings.Join(hosts, " ")
}

// Set populates ProjectsFlag upon flag.Parse()
func (p ProjectsFlag) Set(value string) error {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("%s not in the form of host=repo-a,repo-b,etc", value)
	}
	host := parts[0]
	if _, ok := p[host]; ok {
		return fmt.Errorf("duplicate host: %s", host)
	}
	repos := strings.Split(parts[1], ",")
	p[host] = repos
	return nil
}

// ProjectsFlagToConfig converts Gerrit project configured as command line args
// to prow config struct for backward compatilibity
func ProjectsFlagToConfig(hostProjects ProjectsFlag) map[string]map[string]*config.GerritQueryFilter {
	res := make(map[string]map[string]*config.GerritQueryFilter)
	for host, projects := range hostProjects {
		res[host] = make(map[string]*config.GerritQueryFilter)
		for _, project := range projects {
			res[host][project] = nil
		}
	}
	return res
}

type gerritAuthentication interface {
	SetCookieAuth(name, value string)
}

type gerritAccount interface {
	GetAccount(name string) (*gerrit.AccountInfo, *gerrit.Response, error)
	SetUsername(accountID string, input *gerrit.UsernameInput) (*string, *gerrit.Response, error)
}

type gerritChange interface {
	QueryChanges(opt *gerrit.QueryChangeOptions) (*[]gerrit.ChangeInfo, *gerrit.Response, error)
	SetReview(changeID, revisionID string, input *gerrit.ReviewInput) (*gerrit.ReviewResult, *gerrit.Response, error)
	ListChangeComments(changeID string) (*map[string][]gerrit.CommentInfo, *gerrit.Response, error)
	GetChange(changeId string, opt *gerrit.ChangeOptions) (*ChangeInfo, *gerrit.Response, error)
}

type gerritProjects interface {
	GetBranch(projectName, branchID string) (*gerrit.BranchInfo, *gerrit.Response, error)
}

// gerritInstanceHandler holds all actual gerrit handlers
type gerritInstanceHandler struct {
	instance string
	projects map[string]*config.GerritQueryFilter

	authService    gerritAuthentication
	accountService gerritAccount
	changeService  gerritChange
	projectService gerritProjects

	log logrus.FieldLogger
}

// Client holds a instance:handler map
type Client struct {
	handlers map[string]*gerritInstanceHandler
	// map of instance to gerrit account
	accounts map[string]*gerrit.AccountInfo

	authentication func() (string, error)
	previousToken  string
	lock           sync.RWMutex
}

// ChangeInfo is a gerrit.ChangeInfo
type ChangeInfo = gerrit.ChangeInfo

// RevisionInfo is a gerrit.RevisionInfo
type RevisionInfo = gerrit.RevisionInfo

// FileInfo is a gerrit.FileInfo
type FileInfo = gerrit.FileInfo

// Map from instance name to repos to lastsync time for that repo
type LastSyncState map[string]map[string]time.Time

func (l LastSyncState) DeepCopy() LastSyncState {
	result := LastSyncState{}
	for host, lastSyncs := range l {
		result[host] = map[string]time.Time{}
		for projects, lastSync := range lastSyncs {
			result[host][projects] = lastSync
		}
	}
	return result
}

type roundTripperWithHeader struct {
	upstream http.RoundTripper
}

func (rt *roundTripperWithHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Add("user-agent", "prow")
	// Also include component name
	r.Header.Add("user-agent", "prow/"+version.Name)
	return rt.upstream.RoundTrip(r)
}

// NewClient returns a new gerrit client
func NewClient(instances map[string]map[string]*config.GerritQueryFilter) (*Client, error) {
	c := &Client{
		handlers: map[string]*gerritInstanceHandler{},
		accounts: map[string]*gerrit.AccountInfo{},
	}
	for instance := range instances {
		httpClient := http.Client{
			Transport: &roundTripperWithHeader{upstream: http.DefaultTransport},
		}
		gc, err := gerrit.NewClient(instance, &httpClient)
		if err != nil {
			return nil, err
		}

		c.handlers[instance] = &gerritInstanceHandler{
			instance:       instance,
			projects:       instances[instance],
			authService:    gc.Authentication,
			accountService: gc.Accounts,
			changeService:  gc.Changes,
			projectService: gc.Projects,
			log:            logrus.WithField("host", instance),
		}
	}

	return c, nil
}

func (c *Client) ApplyGlobalConfig(orgRepoConfigGetter func() *config.GerritOrgRepoConfigs, lastSyncTracker *SyncTime, cookiefilePath, tokenPathOverride string, additionalFunc func()) {
	c.applyGlobalConfigOnce(orgRepoConfigGetter, lastSyncTracker, cookiefilePath, tokenPathOverride, additionalFunc)

	go func() {
		for {
			c.applyGlobalConfigOnce(orgRepoConfigGetter, lastSyncTracker, cookiefilePath, tokenPathOverride, additionalFunc)
			// No need to spin constantly, give it a break. It's ok that config change has one second delay.
			time.Sleep(time.Second)
		}
	}()
}

func (c *Client) applyGlobalConfigOnce(orgRepoConfigGetter func() *config.GerritOrgRepoConfigs, lastSyncTracker *SyncTime, cookiefilePath, tokenPathOverride string, additionalFunc func()) {
	orgReposConfig := orgRepoConfigGetter()
	if orgReposConfig == nil {
		return
	}
	// Use globally defined gerrit repos if present
	if err := c.UpdateClients(orgReposConfig.AllRepos()); err != nil {
		logrus.WithError(err).Error("Updating clients.")
	}
	if lastSyncTracker != nil {
		if err := lastSyncTracker.update(orgReposConfig.AllRepos()); err != nil {
			logrus.WithError(err).Error("Syncing states.")
		}
	}

	if additionalFunc != nil {
		additionalFunc()
	}

	// Authenticate creates a goroutine for rotating token secrets when called the first
	// time, afterwards it only authenticate once.
	c.Authenticate(cookiefilePath, tokenPathOverride)
}

func (c *Client) authenticateOnce() {
	c.lock.RLock()
	auth := c.authentication
	c.lock.RUnlock()

	current, err := auth()
	if err != nil {
		logrus.WithError(err).Error("Failed to read gerrit auth token")
	}

	if current == c.previousToken {
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	logrus.Info("New gerrit token, updating handler authentication...")
	c.previousToken = current

	// update auth token for each instance
	for _, handler := range c.handlers {
		handler.authService.SetCookieAuth("o", current)
	}
}

// Authenticate client calls using the specified file.
// Periodically re-reads the file to check for an updated value.
// cookiefilePath takes precedence over tokenPath if both are set.
func (c *Client) Authenticate(cookiefilePath, tokenPath string) {
	var was, auth func() (string, error)
	switch {
	case cookiefilePath != "":
		if tokenPath != "" {
			logrus.WithFields(logrus.Fields{
				"cookiefile": cookiefilePath,
				"token":      tokenPath,
			}).Warn("Ignoring token path in favor of cookiefile")
		}
		auth = func() (string, error) {
			// TODO(fejta): listen for changes
			raw, err := ioutil.ReadFile(cookiefilePath)
			if err != nil {
				return "", fmt.Errorf("read cookie: %w", err)
			}
			fields := strings.Fields(string(raw))
			token := fields[len(fields)-1]
			return token, nil
		}
	case tokenPath != "":
		auth = func() (string, error) {
			raw, err := ioutil.ReadFile(tokenPath)
			if err != nil {
				return "", fmt.Errorf("read token: %w", err)
			}
			return strings.TrimSpace(string(raw)), nil
		}
	default:
		logrus.Info("Using anonymous authentication to gerrit")
		return
	}
	c.lock.Lock()
	was, c.authentication = c.authentication, auth
	c.lock.Unlock()
	c.authenticateOnce() // Ensure requests immediately authenticated
	if was == nil {
		go func() {
			for {
				c.authenticateOnce()
				time.Sleep(time.Minute)
			}
		}()
	}
}

// UpdateClients update gerrit clients with new instances map
func (c *Client) UpdateClients(instances map[string]map[string]*config.GerritQueryFilter) error {
	// Recording in newHandlers, so that deleted instances can be handled.
	newHandlers := make(map[string]*gerritInstanceHandler)
	var errs []error
	c.lock.Lock()
	defer c.lock.Unlock()
	for instance := range instances {
		if handler, ok := c.handlers[instance]; ok {
			// Already initialized, no need to re-initialize handler. But still need
			// to remember to update projects underneath.
			handler.projects = instances[instance]
			newHandlers[instance] = handler
			continue
		}
		gc, err := gerrit.NewClient(instance, nil)
		if err != nil {
			logrus.WithField("instance", instance).WithError(err).Error("Creating gerrit client.")
			errs = append(errs, err)
			continue
		}

		newHandlers[instance] = &gerritInstanceHandler{
			instance:       instance,
			projects:       instances[instance],
			authService:    gc.Authentication,
			accountService: gc.Accounts,
			changeService:  gc.Changes,
			projectService: gc.Projects,
			log:            logrus.WithField("host", instance),
		}
	}
	c.handlers = newHandlers

	return utilerrors.NewAggregate(errs)
}

// QueryChanges queries for all changes from all projects after lastUpdate time
// returns an instance:changes map
func (c *Client) QueryChanges(lastState LastSyncState, rateLimit int) map[string][]ChangeInfo {
	c.lock.RLock()
	defer c.lock.RUnlock()
	result := map[string][]ChangeInfo{}
	for _, h := range c.handlers {
		lastStateForInstance := lastState[h.instance]
		changes := h.queryAllChanges(lastStateForInstance, rateLimit)
		if len(changes) == 0 {
			continue
		}

		result[h.instance] = append(result[h.instance], changes...)
	}
	return result
}

func (c *Client) QueryChangesForInstance(instance string, lastState LastSyncState, rateLimit int) []ChangeInfo {
	c.lock.RLock()
	defer c.lock.RUnlock()
	h, ok := c.handlers[instance]
	if !ok {
		logrus.WithField("instance", instance).WithField("laststate", lastState).Warn("Instance not registered as handlers.")
		return []ChangeInfo{}
	}
	lastStateForInstance := lastState[instance]
	return h.queryAllChanges(lastStateForInstance, rateLimit)
}

// QueryChangesForProject queries change for a project.
//
// Important: this method does not update LastSyncState as it is per instance
// based. It doesn't make sense to update the state as this method has no idea
// whether all other projects have been queries or not yet. So caller of this
// method is responsible for making sure that LastSyncState is up-to-date, if
// the lastUpdate time is used by caller.
func (c *Client) QueryChangesForProject(instance, project string, lastUpdate time.Time, rateLimit int, additionalFilters ...string) ([]ChangeInfo, error) {
	log := logrus.WithContext(context.Background()).WithField("instance", instance)

	c.lock.RLock()
	defer c.lock.RUnlock()
	h, ok := c.handlers[instance]
	if !ok {
		return []ChangeInfo{}, fmt.Errorf("instance handler for %q not found, it might not have been initialized yet", instance)
	}

	queryFilters, ok := h.projects[project]
	if !ok {
		return []ChangeInfo{}, fmt.Errorf("project %q from instance %q not registered in gerrit handler, it might not have been initialized yet", project, instance)
	}

	changes, err := h.QueryChangesForProject(log, project, lastUpdate, rateLimit, append(queryStringsFromQueryFilter(queryFilters), additionalFilters...)...)
	if err != nil {
		return []ChangeInfo{}, fmt.Errorf("failed to query changes for project %q of %q instance: %v", project, instance, err)
	}
	return changes, nil
}

func (c *Client) GetChange(instance, id string) (*ChangeInfo, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	h, ok := c.handlers[instance]
	if !ok {
		return nil, fmt.Errorf("not activated gerrit instance: %s", instance)
	}

	info, resp, err := h.changeService.GetChange(id, nil)
	if err != nil {
		return nil, fmt.Errorf("error getting current change: %w", responseBodyError(err, resp))
	}

	return info, nil
}

func (c *Client) ChangeExist(instance, id string) (bool, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	h, ok := c.handlers[instance]
	if !ok {
		return false, fmt.Errorf("not activated gerrit instance: %s", instance)
	}

	_, resp, err := h.changeService.GetChange(id, nil)
	if err != nil {
		if resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("error getting current change: %w", responseBodyError(err, resp))
	}

	return true, nil
}

// responseBodyError returns the error with the response body text appended if there is any.
func responseBodyError(err error, resp *gerrit.Response) error {
	if resp == nil || resp.Response == nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := ioutil.ReadAll(resp.Body) // Ignore the error since this is best effort.
	return fmt.Errorf("%w, response body: %q", err, string(b))
}

// SetReview writes a review comment base on the change id + revision
func (c *Client) SetReview(instance, id, revision, message string, labels map[string]string) error {
	c.lock.RLock()
	defer c.lock.RUnlock()
	h, ok := c.handlers[instance]
	if !ok {
		return fmt.Errorf("not activated gerrit instance: %s", instance)
	}

	if _, resp, err := h.changeService.SetReview(id, revision, &gerrit.ReviewInput{
		Message: message,
		Labels:  labels,
	}); err != nil {
		return fmt.Errorf("cannot comment to gerrit: %w", responseBodyError(err, resp))
	}

	return nil
}

// GetBranchRevision returns SHA of HEAD of a branch
func (c *Client) GetBranchRevision(instance, project, branch string) (string, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	h, ok := c.handlers[instance]
	if !ok {
		return "", fmt.Errorf("not activated gerrit instance: %s", instance)
	}

	res, resp, err := h.projectService.GetBranch(project, branch)
	if err != nil {
		return "", responseBodyError(err, resp)
	}

	return res.Revision, nil
}

// Account returns gerrit account for the given instance
func (c *Client) Account(instance string) (*gerrit.AccountInfo, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if existing, ok := c.accounts[instance]; ok {
		return existing, nil
	}

	handler, ok := c.handlers[instance]
	if !ok {
		return nil, errors.New("no handlers found")
	}

	self, resp, err := handler.accountService.GetAccount("self")
	if err != nil {
		return nil, fmt.Errorf("GetAccount() failed with new authentication: %w", responseBodyError(err, resp))

	}
	c.accounts[instance] = self
	return c.accounts[instance], nil
}

// private handler implementation details

func (h *gerritInstanceHandler) queryAllChanges(lastState map[string]time.Time, rateLimit int) []gerrit.ChangeInfo {
	result := []gerrit.ChangeInfo{}
	timeNow := time.Now()
	for project, filters := range h.projects {
		log := h.log.WithField("repo", project)
		lastUpdate, ok := lastState[project]
		if !ok {
			lastUpdate = timeNow
			log.WithField("now", timeNow).Warn("lastState not found, defaulting to now")
		}
		changes, err := h.QueryChangesForProject(log, project, lastUpdate, rateLimit, queryStringsFromQueryFilter(filters)...)
		if err != nil {
			clientMetrics.queryResults.WithLabelValues(h.instance, project, ResultError).Inc()
			// don't halt on error from one project, log & continue
			log.WithError(err).WithFields(logrus.Fields{
				"lastUpdate": lastUpdate,
				"rateLimit":  rateLimit,
			}).Error("Failed to query changes")
			continue
		}
		clientMetrics.queryResults.WithLabelValues(h.instance, project, ResultSuccess).Inc()
		result = append(result, changes...)
	}

	return result
}

func parseStamp(value gerrit.Timestamp) time.Time {
	return value.Time
}

func (h *gerritInstanceHandler) injectPatchsetMessages(change *gerrit.ChangeInfo) error {
	out, _, err := h.changeService.ListChangeComments(change.ID)
	if err != nil {
		return err
	}
	outer := *out
	comments, ok := outer["/PATCHSET_LEVEL"]
	if !ok {
		return nil
	}
	var changed bool
	for _, c := range comments {
		change.Messages = append(change.Messages, gerrit.ChangeMessageInfo{
			Author:         c.Author,
			Date:           *c.Updated,
			Message:        c.Message,
			RevisionNumber: c.PatchSet,
		})
		changed = true
	}
	if changed {
		sort.SliceStable(change.Messages, func(i, j int) bool {
			return change.Messages[i].Date.Before(change.Messages[j].Date.Time)
		})
	}
	return nil
}

func queryStringsFromQueryFilter(filters *config.GerritQueryFilter) []string {
	if filters == nil {
		return nil
	}

	var res []string

	var branchFilter []string
	for _, br := range filters.Branches {
		branchFilter = append(branchFilter, fmt.Sprintf("branch:%s", br))
	}
	if len(branchFilter) > 0 {
		res = append(res, fmt.Sprintf("(%s)", strings.Join(branchFilter, "+OR+")))
	}
	var excludedBranchFilter []string
	for _, br := range filters.ExcludedBranches {
		excludedBranchFilter = append(excludedBranchFilter, fmt.Sprintf("-branch:%s", br))
	}
	if len(excludedBranchFilter) > 0 {
		res = append(res, fmt.Sprintf("(%s)", strings.Join(excludedBranchFilter, "+AND+")))
	}

	return res
}

func (h *gerritInstanceHandler) QueryChangesForProject(log logrus.FieldLogger, project string, lastUpdate time.Time, rateLimit int, additionalFilters ...string) ([]gerrit.ChangeInfo, error) {
	var pending []gerrit.ChangeInfo

	var opt gerrit.QueryChangeOptions
	opt.Query = append(opt.Query, strings.Join(append(additionalFilters, "project:"+project), "+"))
	opt.AdditionalFields = []string{"CURRENT_REVISION", "CURRENT_COMMIT", "CURRENT_FILES", "MESSAGES"}

	log = log.WithFields(logrus.Fields{"query": opt.Query, "additional_fields": opt.AdditionalFields})
	var start int

	for {
		opt.Limit = rateLimit
		opt.Start = start

		// override log just for this for loop
		log := log.WithField("start", opt.Start)
		// The change output is sorted by the last update time, most recently updated to oldest updated.
		// Gerrit API docs: https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#list-changes
		changes, resp, err := h.changeService.QueryChanges(&opt)
		if err != nil {
			// should not happen? Let next sync loop catch up
			return nil, responseBodyError(err, resp)
		}

		if changes == nil || len(*changes) == 0 {
			log.Info("No more changes")
			return pending, nil
		}

		log.WithField("changes", len(*changes)).Debug("Found gerrit changes from page.")

		start += len(*changes)

		for _, change := range *changes {
			// if we already processed this change, then we stop the current sync loop
			updated := parseStamp(change.Updated)

			log := log.WithFields(logrus.Fields{
				"change":     change.Number,
				"updated":    change.Updated,
				"status":     change.Status,
				"lastUpdate": lastUpdate,
			})

			// stop when we find a change last updated before lastUpdate
			if !updated.After(lastUpdate) {
				log.Debug("No more recently updated changes")
				return pending, nil
			}

			// process recently updated change
			switch change.Status {
			case Merged:
				submitted := parseStamp(*change.Submitted)
				log := log.WithField("submitted", submitted)
				if !submitted.After(lastUpdate) {
					log.Debug("Skipping previously merged change")
					continue
				}
				log.Debug("Found merged change")
				pending = append(pending, change)
			case New:
				// we need to make sure the change update is from a fresh commit change
				rev, ok := change.Revisions[change.CurrentRevision]
				if !ok {
					log.WithError(err).WithField("revision", change.CurrentRevision).Error("Revision not found")
					continue
				}

				created := parseStamp(rev.Created)
				log := log.WithField("created", created)
				if err := h.injectPatchsetMessages(&change); err != nil {
					log.WithError(err).Error("Failed to inject patchset messages")
				}
				changeMessages := change.Messages
				var newMessages bool

				for _, message := range changeMessages {
					if message.RevisionNumber == rev.Number {
						messageTime := parseStamp(message.Date)
						if messageTime.After(lastUpdate) {
							log.WithFields(logrus.Fields{
								"message":     message.Message,
								"messageDate": messageTime,
							}).Info("New messages")
							newMessages = true
							break
						}
					}
				}

				if !newMessages && !created.After(lastUpdate) {
					// stale commit
					log.Debug("Skipping existing change")
					continue
				}
				if !newMessages {
					log.Debug("Found updated change")
				}
				pending = append(pending, change)
			default:
				// change has been abandoned, do nothing
				log.Debug("Ignored change")
			}
		}
	}
}
