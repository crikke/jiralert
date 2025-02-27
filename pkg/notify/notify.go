// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package notify

import (
	"bytes"
	"crypto/sha512"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/andygrunwald/go-jira"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus-community/jiralert/pkg/alertmanager"
	"github.com/prometheus-community/jiralert/pkg/config"
	"github.com/prometheus-community/jiralert/pkg/template"
	"github.com/trivago/tgo/tcontainer"
)

// TODO(bwplotka): Consider renaming this package to ticketer.

type jiraIssueService interface {
	Search(jql string, options *jira.SearchOptions) ([]jira.Issue, *jira.Response, error)
	GetTransitions(id string) ([]jira.Transition, *jira.Response, error)

	Create(issue *jira.Issue) (*jira.Issue, *jira.Response, error)
	UpdateWithOptions(issue *jira.Issue, opts *jira.UpdateQueryOptions) (*jira.Issue, *jira.Response, error)
	DoTransition(ticketID, transitionID string) (*jira.Response, error)
}

// Receiver wraps a specific Alertmanager receiver with its configuration and templates, creating/updating/reopening Jira issues based on Alertmanager notifications.
type Receiver struct {
	logger log.Logger
	client jiraIssueService
	// TODO(bwplotka): Consider splitting receiver config with ticket service details.
	conf *config.ReceiverConfig
	tmpl *template.Template

	timeNow func() time.Time
}

// NewReceiver creates a Receiver using the provided configuration, template and jiraIssueService.
func NewReceiver(logger log.Logger, c *config.ReceiverConfig, t *template.Template, client jiraIssueService) *Receiver {
	return &Receiver{logger: logger, conf: c, tmpl: t, client: client, timeNow: time.Now}
}

// transforms alertmanager.Data to alertmanager.Data slice grouped by Alert
func (r *Receiver) toAlert(d *alertmanager.Data) []alertmanager.Data {

	slice := make([]alertmanager.Data, 0)
	for _, a := range d.Alerts {

		data := alertmanager.Data{
			GroupKey:          d.GroupKey,
			GroupLabels:       d.GroupLabels,
			Status:            a.Status,
			CommonLabels:      a.Labels,
			CommonAnnotations: a.Annotations,
			ExternalURL:       d.ExternalURL,
			Alerts:            []alertmanager.Alert{a},
			Version:           d.Version,
			Receiver:          d.Receiver,
		}
		slice = append(slice, data)
	}

	return slice
}

// transforms alertmanager.Data to alertmanager.Data slice grouped by AlertRule
func (r *Receiver) toAlertRule(d *alertmanager.Data) []alertmanager.Data {

	alertsData := make(map[string]alertmanager.Data)

	for _, alert := range d.Alerts {

		name, ok := alert.Labels["alertname"]

		if !ok {
			continue
		}

		data, ok := alertsData[name]
		if !ok {
			data = alertmanager.Data{
				GroupKey:          d.GroupKey,
				GroupLabels:       d.GroupLabels,
				Status:            alertmanager.AlertResolved,
				ExternalURL:       d.ExternalURL,
				Alerts:            make(alertmanager.Alerts, 0),
				CommonAnnotations: make(alertmanager.KV),
				CommonLabels:      make(alertmanager.KV),
			}
		}

		data.Alerts = append(data.Alerts, alert)

		if alert.Status == alertmanager.AlertFiring {
			data.Status = alertmanager.AlertFiring
		}

		alertsData[name] = data
	}

	slice := make([]alertmanager.Data, len(alertsData))
	// update common labels and annotations
	// https://github.com/prometheus/alertmanager/blob/main/template/template.go#L331
	idx := 0
	for _, data := range alertsData {
		if len(data.Alerts) >= 1 {
			var (
				commonLabels      = data.Alerts[0].Labels
				commonAnnotations = data.Alerts[0].Annotations
			)
			for _, a := range data.Alerts[1:] {
				if len(commonLabels) == 0 && len(commonAnnotations) == 0 {
					break
				}
				for ln, lv := range commonLabels {
					if a.Labels[ln] != lv {
						delete(commonLabels, ln)
					}
				}
				for an, av := range commonAnnotations {
					if a.Annotations[an] != av {
						delete(commonAnnotations, an)
					}
				}
			}
			for k, v := range commonLabels {
				data.CommonLabels[string(k)] = string(v)
			}
			for k, v := range commonAnnotations {
				data.CommonAnnotations[string(k)] = string(v)
			}
		}
		slice[idx] = data
		idx++
	}

	return slice
}

func (r *Receiver) Notify(data *alertmanager.Data, hashJiraLabel bool) (bool, error) {

	var slice []alertmanager.Data
	switch r.conf.GroupIssueBy {
	// by default alerts are already grouped by AlertGroup, so no transformation is needed here
	case config.AlertGroup, "":
		slice = []alertmanager.Data{*data}
	case config.AlertRule:
		slice = r.toAlertRule(data)
	case config.Alert:
		slice = r.toAlert(data)
	}

	for _, d := range slice {
		retry, err := r.notify(&d, hashJiraLabel)
		if err != nil {
			return retry, err
		}
	}

	return false, nil
}

// Notify manages JIRA issues based on alertmanager webhook notify message.
func (r *Receiver) notify(data *alertmanager.Data, hashJiraLabel bool) (bool, error) {
	project, err := r.tmpl.Execute(r.conf.Project, data)
	if err != nil {
		return false, errors.Wrap(err, "generate project from template")
	}

	labels := make([]string, 0)

	if r.conf.AddCommonLabels {
		for _, pair := range data.CommonLabels.SortedPairs() {
			labels = append(labels, fmt.Sprintf("%s=%q", pair.Name, pair.Value))
		}
	}

	idLabel, err := r.toIssueIdentifierLabel(data, hashJiraLabel)
	if err != nil {
		return false, errors.Wrap(err, "build IssueIdentifierLabel")
	}

	labels = append(labels, idLabel)
	issue, retry, err := r.findIssueToReuse(project, idLabel)
	if err != nil {
		return retry, err
	}

	// We want up to date title no matter what.
	// This allows reflecting current group state if desired by user e.g {{ len $.Alerts.Firing() }}
	issueSummary, err := r.tmpl.Execute(r.conf.Summary, data)
	if err != nil {
		return false, errors.Wrap(err, "generate summary from template")
	}

	issueDesc, err := r.tmpl.Execute(r.conf.Description, data)
	if err != nil {
		return false, errors.Wrap(err, "render issue description")
	}

	if issue != nil {
		// Update summary if needed.
		if issue.Fields.Summary != issueSummary {
			retry, err := r.updateSummary(issue.Key, issueSummary)
			if err != nil {
				return retry, err
			}
		}

		if issue.Fields.Description != issueDesc {
			retry, err := r.updateDescription(issue.Key, issueDesc)
			if err != nil {
				return retry, err
			}
		}

		if len(data.Alerts.Firing()) == 0 {
			if r.conf.AutoResolve != nil {
				level.Debug(r.logger).Log("msg", "no firing alert; resolving issue", "key", issue.Key, "label", labels)
				retry, err := r.resolveIssue(issue.Key)
				if err != nil {
					return retry, err
				}
				return false, nil
			}

			level.Debug(r.logger).Log("msg", "no firing alert; summary checked, nothing else to do.", "key", issue.Key, "label", labels)
			return false, nil
		}

		// The set of JIRA status categories is fixed, this is a safe check to make.
		if issue.Fields.Status.StatusCategory.Key != "done" {
			level.Debug(r.logger).Log("msg", "issue is unresolved, all is done", "key", issue.Key, "label", labels)
			return false, nil
		}

		if r.conf.WontFixResolution != "" && issue.Fields.Resolution != nil &&
			issue.Fields.Resolution.Name == r.conf.WontFixResolution {
			level.Info(r.logger).Log("msg", "issue was resolved as won't fix, not reopening", "key", issue.Key, "label", labels, "resolution", issue.Fields.Resolution.Name)
			return false, nil
		}

		level.Info(r.logger).Log("msg", "issue was recently resolved, reopening", "key", issue.Key, "label", labels)
		return r.reopen(issue.Key)
	}

	if len(data.Alerts.Firing()) == 0 {
		level.Debug(r.logger).Log("msg", "no firing alert; nothing to do.", "label", labels)
		return false, nil
	}

	level.Info(r.logger).Log("msg", "no recent matching issue found, creating new issue", "label", labels)

	issueType, err := r.tmpl.Execute(r.conf.IssueType, data)
	if err != nil {
		return false, errors.Wrap(err, "render issue type")
	}

	issue = &jira.Issue{
		Fields: &jira.IssueFields{
			Project:     jira.Project{Key: project},
			Type:        jira.IssueType{Name: issueType},
			Description: issueDesc,
			Summary:     issueSummary,
			Labels:      labels,
			Unknowns:    tcontainer.NewMarshalMap(),
		},
	}
	if r.conf.Priority != "" {
		issuePrio, err := r.tmpl.Execute(r.conf.Priority, data)
		if err != nil {
			return false, errors.Wrap(err, "render issue priority")
		}

		issue.Fields.Priority = &jira.Priority{Name: issuePrio}
	}

	if len(r.conf.Components) > 0 {
		issue.Fields.Components = make([]*jira.Component, 0, len(r.conf.Components))
		for _, component := range r.conf.Components {
			issueComp, err := r.tmpl.Execute(component, data)
			if err != nil {
				return false, errors.Wrap(err, "render issue component")
			}

			issue.Fields.Components = append(issue.Fields.Components, &jira.Component{Name: issueComp})
		}
	}

	if r.conf.AddGroupLabels {
		for k, v := range data.GroupLabels {
			issue.Fields.Labels = append(issue.Fields.Labels, fmt.Sprintf("%s=%q", k, v))
		}
	}

	for key, value := range r.conf.Fields {
		issue.Fields.Unknowns[key], err = deepCopyWithTemplate(value, r.tmpl, data)
		if err != nil {
			return false, err
		}
	}

	return r.create(issue)
}

// deepCopyWithTemplate returns a deep copy of a map/slice/array/string/int/bool or combination thereof, executing the
// provided template (with the provided data) on all string keys or values. All maps are connverted to
// map[string]interface{}, with all non-string keys discarded.
func deepCopyWithTemplate(value interface{}, tmpl *template.Template, data interface{}) (interface{}, error) {
	if value == nil {
		return value, nil
	}

	valueMeta := reflect.ValueOf(value)
	switch valueMeta.Kind() {

	case reflect.String:
		return tmpl.Execute(value.(string), data)

	case reflect.Array, reflect.Slice:
		arrayLen := valueMeta.Len()
		converted := make([]interface{}, arrayLen)
		for i := 0; i < arrayLen; i++ {
			var err error
			converted[i], err = deepCopyWithTemplate(valueMeta.Index(i).Interface(), tmpl, data)
			if err != nil {
				return nil, err
			}
		}
		return converted, nil

	case reflect.Map:
		keys := valueMeta.MapKeys()
		converted := make(map[string]interface{}, len(keys))

		for _, keyMeta := range keys {
			var err error
			strKey, isString := keyMeta.Interface().(string)
			if !isString {
				continue
			}
			strKey, err = tmpl.Execute(strKey, data)
			if err != nil {
				return nil, err
			}
			converted[strKey], err = deepCopyWithTemplate(valueMeta.MapIndex(keyMeta).Interface(), tmpl, data)
			if err != nil {
				return nil, err
			}
		}
		return converted, nil
	default:
		return value, nil
	}
}

func (r *Receiver) toIssueIdentifierLabel(data *alertmanager.Data, hashJiraLabel bool) (string, error) {

	// if toIssueIdentifierLabel not set, fallback to old behavior
	if r.conf.IssueIdentifierLabel == "" {
		return toGroupTicketLabel(data.GroupLabels, hashJiraLabel), nil
	}

	label, err := r.tmpl.Execute(r.conf.IssueIdentifierLabel, data)
	if err != nil {
		return "", err
	}

	return strings.Replace(label, " ", "", -1), nil
}

// toGroupTicketLabel returns the group labels as a single string.
// This is used to reference each ticket groups.
// (old) default behavior: String is the form of an ALERT Prometheus metric name, with all spaces removed.
// new opt-in behavior: String is the form of JIRALERT{sha512hash(groupLabels)}
// hashing ensures that JIRA validation still accepts the output even
// if the combined length of all groupLabel key-value pairs would be
// longer than 255 chars
func toGroupTicketLabel(labels alertmanager.KV, hashJiraLabel bool) string {

	// new opt in behavior
	if hashJiraLabel {
		hash := sha512.New()
		for _, p := range labels.SortedPairs() {
			kvString := fmt.Sprintf("%s=%q,", p.Name, p.Value)
			_, _ = hash.Write([]byte(kvString)) // hash.Write can never return an error
		}
		return fmt.Sprintf("JIRALERT{%x}", hash.Sum(nil))
	}

	// old default behavior
	buf := bytes.NewBufferString("ALERT{")
	for _, p := range labels.SortedPairs() {
		buf.WriteString(p.Name)
		buf.WriteString(fmt.Sprintf("=%q,", p.Value))
	}
	buf.Truncate(buf.Len() - 1)
	buf.WriteString("}")
	return strings.Replace(buf.String(), " ", "", -1)
}

func (r *Receiver) search(project, issueLabel string) (*jira.Issue, bool, error) {
	query := fmt.Sprintf("project=\"%s\" and labels=%q order by resolutiondate desc", project, issueLabel)
	options := &jira.SearchOptions{
		Fields:     []string{"summary", "status", "resolution", "resolutiondate"},
		MaxResults: 2,
	}

	level.Debug(r.logger).Log("msg", "search", "query", query, "options", fmt.Sprintf("%+v", options))
	issues, resp, err := r.client.Search(query, options)
	if err != nil {
		retry, err := handleJiraErrResponse("Issue.Search", resp, err, r.logger)
		return nil, retry, err
	}

	if len(issues) == 0 {
		level.Debug(r.logger).Log("msg", "no results", "query", query)
		return nil, false, nil
	}

	issue := issues[0]
	if len(issues) > 1 {
		level.Warn(r.logger).Log("msg", "more than one issue matched, picking most recently resolved", "query", query, "issues", issues, "picked", issue)
	}

	level.Debug(r.logger).Log("msg", "found", "issue", issue, "query", query)
	return &issue, false, nil
}

func (r *Receiver) findIssueToReuse(project string, issueGroupLabel string) (*jira.Issue, bool, error) {
	issue, retry, err := r.search(project, issueGroupLabel)
	if err != nil {
		return nil, retry, err
	}

	if issue == nil {
		return nil, false, nil
	}

	resolutionTime := time.Time(issue.Fields.Resolutiondate)
	if resolutionTime != (time.Time{}) && resolutionTime.Add(time.Duration(*r.conf.ReopenDuration)).Before(r.timeNow()) && *r.conf.ReopenDuration != 0 {
		level.Debug(r.logger).Log("msg", "existing resolved issue is too old to reopen, skipping", "key", issue.Key, "label", issueGroupLabel, "resolution_time", resolutionTime.Format(time.RFC3339), "reopen_duration", *r.conf.ReopenDuration)
		return nil, false, nil
	}

	// Reuse issue.
	return issue, false, nil
}

func (r *Receiver) updateSummary(issueKey string, summary string) (bool, error) {
	level.Debug(r.logger).Log("msg", "updating issue with new summary", "key", issueKey, "summary", summary)

	issueUpdate := &jira.Issue{
		Key: issueKey,
		Fields: &jira.IssueFields{
			Summary: summary,
		},
	}
	issue, resp, err := r.client.UpdateWithOptions(issueUpdate, nil)
	if err != nil {
		return handleJiraErrResponse("Issue.UpdateWithOptions", resp, err, r.logger)
	}
	level.Debug(r.logger).Log("msg", "issue summary updated", "key", issue.Key, "id", issue.ID)
	return false, nil
}

func (r *Receiver) updateDescription(issueKey string, description string) (bool, error) {
	level.Debug(r.logger).Log("msg", "updating issue with new description", "key", issueKey, "description", description)

	issueUpdate := &jira.Issue{
		Key: issueKey,
		Fields: &jira.IssueFields{
			Description: description,
		},
	}
	issue, resp, err := r.client.UpdateWithOptions(issueUpdate, nil)
	if err != nil {
		return handleJiraErrResponse("Issue.UpdateWithOptions", resp, err, r.logger)
	}
	level.Debug(r.logger).Log("msg", "issue summary updated", "key", issue.Key, "id", issue.ID)
	return false, nil
}

func (r *Receiver) reopen(issueKey string) (bool, error) {
	return r.doTransition(issueKey, r.conf.ReopenState)
}

func (r *Receiver) create(issue *jira.Issue) (bool, error) {
	level.Debug(r.logger).Log("msg", "create", "issue", fmt.Sprintf("%+v", *issue.Fields))
	newIssue, resp, err := r.client.Create(issue)
	if err != nil {
		return handleJiraErrResponse("Issue.Create", resp, err, r.logger)
	}
	*issue = *newIssue

	level.Info(r.logger).Log("msg", "issue created", "key", issue.Key, "id", issue.ID)
	return false, nil
}

func handleJiraErrResponse(api string, resp *jira.Response, err error, logger log.Logger) (bool, error) {
	if resp == nil || resp.Request == nil {
		level.Debug(logger).Log("msg", "handleJiraErrResponse", "api", api, "err", err)
	} else {
		level.Debug(logger).Log("msg", "handleJiraErrResponse", "api", api, "err", err, "url", resp.Request.URL)
	}

	if resp != nil && resp.StatusCode/100 != 2 {
		retry := resp.StatusCode == 500 || resp.StatusCode == 503
		body, _ := io.ReadAll(resp.Body)
		// go-jira error message is not particularly helpful, replace it
		return retry, errors.Errorf("JIRA request %s returned status %s, body %q", resp.Request.URL, resp.Status, string(body))
	}
	return false, errors.Wrapf(err, "JIRA request %s failed", api)
}

func (r *Receiver) resolveIssue(issueKey string) (bool, error) {
	return r.doTransition(issueKey, r.conf.AutoResolve.State)
}

func (r *Receiver) doTransition(issueKey string, transitionState string) (bool, error) {
	transitions, resp, err := r.client.GetTransitions(issueKey)
	if err != nil {
		return handleJiraErrResponse("Issue.GetTransitions", resp, err, r.logger)
	}

	for _, t := range transitions {
		if t.Name == transitionState {
			level.Debug(r.logger).Log("msg", fmt.Sprintf("transition %s", transitionState), "key", issueKey, "transitionID", t.ID)
			resp, err = r.client.DoTransition(issueKey, t.ID)
			if err != nil {
				return handleJiraErrResponse("Issue.DoTransition", resp, err, r.logger)
			}

			level.Debug(r.logger).Log("msg", transitionState, "key", issueKey)
			return false, nil
		}
	}
	return false, errors.Errorf("JIRA state %q does not exist or no transition possible for %s", r.conf.ReopenState, issueKey)

}
