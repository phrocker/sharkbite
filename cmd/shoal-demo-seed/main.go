// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

// Command shoal-demo-seed provisions the deterministic two-user demo scenario
// through Shoal's authenticated HTTP APIs.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	scenarioSchema = "shoal.demo/v1"
	activityDays   = 14
	workItemCount  = 7
)

var tokenEnvironmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

type config struct {
	BaseURL       string          `json:"base_url"`
	OIDCIssuer    string          `json:"oidc_issuer"`
	ScenarioStart string          `json:"scenario_start"`
	Team          teamConfig      `json:"team"`
	Graph         graphConfig     `json:"graph"`
	Workspace     workspaceConfig `json:"workspace"`
	Users         []userConfig    `json:"users"`
}

type teamConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type graphConfig struct {
	Namespace           string `json:"namespace"`
	AuthorizationDomain string `json:"authorization_domain"`
	SourceID            string `json:"source_id"`
	PolicyID            string `json:"policy_id"`
}

type workspaceConfig struct {
	RetrievalTopK uint32 `json:"retrieval_top_k"`
	GraphDepth    uint32 `json:"graph_depth"`
	GraphFanout   uint32 `json:"graph_fanout"`
	GraphNodes    uint32 `json:"graph_nodes"`
	OutputBytes   uint64 `json:"output_bytes"`
}

type userConfig struct {
	PersonID     string      `json:"person_id"`
	DisplayName  string      `json:"display_name"`
	TeamRole     string      `json:"team_role"`
	OIDCSubject  string      `json:"oidc_subject_id"`
	WorkspaceID  string      `json:"workspace_id"`
	TokenEnv     string      `json:"token_env"`
	Agent        agentConfig `json:"agent"`
	trustedID    string
	workspaceKey string
}

type agentConfig struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Capability         string   `json:"capability"`
	Skills             []string `json:"skills"`
	ExecutorRef        string   `json:"executor_ref"`
	RegistrationKeyEnv string   `json:"registration_key_env"`
}

type fixtureFile struct {
	Name    string
	Content []byte
}

type userPlan struct {
	User  userConfig
	Files []fixtureFile
}

type scenario struct {
	Plans     []userPlan
	Nodes     []webapi.GraphMaterializeNode
	Relations []webapi.GraphMaterializeRelation
}

type provisioner struct {
	client *http.Client
	getenv func(string) string
}

type provisionSummary struct {
	TeamID            string        `json:"team_id"`
	WorkItems         int           `json:"work_items"`
	ActivityDays      int           `json:"activity_days"`
	UserSummaries     []userSummary `json:"users"`
	GraphDisposition  string        `json:"graph_disposition"`
	TeamNodeID        string        `json:"team_node_id"`
	OverviewPeople    int           `json:"overview_people"`
	OverviewAgents    int           `json:"overview_agents"`
	OverviewWorkItems int           `json:"overview_work_items"`
}

type userSummary struct {
	PersonID        string            `json:"person_id"`
	WorkspaceID     string            `json:"workspace_id"`
	WorkspaceHeader string            `json:"workspace_header"`
	Files           map[string]string `json:"files"`
}

type identityResponse struct {
	Authenticated bool     `json:"authenticated"`
	Subject       string   `json:"subject"`
	Operations    []string `json:"operations"`
}

type ingestResponse struct {
	Snapshot webapi.Snapshot `json:"snapshot"`
	Files    []struct {
		Name        string `json:"name"`
		Disposition string `json:"disposition"`
	} `json:"files"`
}

type fleetDescriptorResponse struct {
	ID         string    `json:"id"`
	Generation int64     `json:"generation"`
	Lease      time.Time `json:"lease_expires_at"`
}

func main() {
	if err := run(
		context.Background(), os.Args[1:], os.Stdout,
		os.Getenv, &http.Client{Timeout: 30 * time.Second},
	); err != nil {
		fmt.Fprintf(os.Stderr, "shoal-demo-seed: %v\n", err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	args []string,
	output io.Writer,
	getenv func(string) string,
	client *http.Client,
) error {
	flags := flag.NewFlagSet("shoal-demo-seed", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to the demo scenario JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	loaded, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	generated, err := buildScenario(loaded)
	if err != nil {
		return err
	}
	if client == nil || getenv == nil {
		return errors.New("HTTP client and environment reader are required")
	}
	summary, err := (&provisioner{client: client, getenv: getenv}).provision(
		ctx, loaded, generated)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func loadConfig(path string) (config, error) {
	file, err := os.Open(path)
	if err != nil {
		return config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var value config
	if err := decoder.Decode(&value); err != nil {
		return config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return config{}, err
	}
	if err := value.validate(); err != nil {
		return config{}, err
	}
	return value, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("config contains more than one JSON value")
		}
		return fmt.Errorf("decode trailing config: %w", err)
	}
	return nil
}

func (c *config) validate() error {
	base, err := validateEndpoint(c.BaseURL)
	if err != nil {
		return fmt.Errorf("base_url: %w", err)
	}
	c.BaseURL = strings.TrimSuffix(base.String(), "/")
	issuer, err := validateIssuer(c.OIDCIssuer)
	if err != nil {
		return fmt.Errorf("oidc_issuer: %w", err)
	}
	c.OIDCIssuer = issuer
	start, err := time.Parse(time.RFC3339, c.ScenarioStart)
	if err != nil || start.Location() != time.UTC {
		return errors.New("scenario_start must be an RFC3339 UTC timestamp ending in Z")
	}
	if start.Nanosecond() != 0 {
		return errors.New("scenario_start must not contain fractional seconds")
	}
	c.ScenarioStart = start.Format(time.RFC3339)
	if err := validateID("team.id", c.Team.ID); err != nil {
		return err
	}
	if strings.TrimSpace(c.Team.Name) == "" {
		return errors.New("team.name is required")
	}
	for name, value := range map[string]string{
		"graph.namespace":            c.Graph.Namespace,
		"graph.authorization_domain": c.Graph.AuthorizationDomain,
		"graph.source_id":            c.Graph.SourceID,
		"graph.policy_id":            c.Graph.PolicyID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := c.Workspace.validate(); err != nil {
		return err
	}
	if len(c.Users) != 2 {
		return errors.New("users must contain exactly two people")
	}
	seenPeople := make(map[string]struct{}, len(c.Users))
	seenSubjects := make(map[string]struct{}, len(c.Users))
	seenWorkspaces := make(map[string]struct{}, len(c.Users))
	seenTokens := make(map[string]struct{}, len(c.Users))
	seenAgents := make(map[string]struct{}, len(c.Users))
	for index := range c.Users {
		user := &c.Users[index]
		prefix := fmt.Sprintf("users[%d]", index)
		if err := validateID(prefix+".person_id", user.PersonID); err != nil {
			return err
		}
		if _, duplicate := seenPeople[user.PersonID]; duplicate {
			return errors.New("users must have distinct person_id values")
		}
		seenPeople[user.PersonID] = struct{}{}
		if strings.TrimSpace(user.DisplayName) == "" {
			return fmt.Errorf("%s.display_name is required", prefix)
		}
		if strings.TrimSpace(user.TeamRole) == "" {
			return fmt.Errorf("%s.team_role is required", prefix)
		}
		if err := validateID(prefix+".oidc_subject_id", user.OIDCSubject); err != nil {
			return err
		}
		// shoal-explore-web refuses a sub containing '#' (#546): under
		// oidc:<iss>#<sub> it could read as a non-sub subject claim's
		// oidc:<iss>#<tag>#<value>, and no token could ever mint it.
		if strings.Contains(user.OIDCSubject, "#") {
			return fmt.Errorf("%s.oidc_subject_id must not contain '#'", prefix)
		}
		if _, duplicate := seenSubjects[user.OIDCSubject]; duplicate {
			return errors.New("users must have distinct oidc_subject_id values")
		}
		seenSubjects[user.OIDCSubject] = struct{}{}
		if err := validateID(prefix+".workspace_id", user.WorkspaceID); err != nil {
			return err
		}
		if _, duplicate := seenWorkspaces[user.WorkspaceID]; duplicate {
			return errors.New("users must have distinct workspace_id values")
		}
		seenWorkspaces[user.WorkspaceID] = struct{}{}
		if !tokenEnvironmentName.MatchString(user.TokenEnv) {
			return fmt.Errorf(
				"%s.token_env must be an uppercase environment variable name",
				prefix,
			)
		}
		if _, duplicate := seenTokens[user.TokenEnv]; duplicate {
			return errors.New("users must have distinct token_env values")
		}
		seenTokens[user.TokenEnv] = struct{}{}
		if err := validateID(prefix+".agent.id", user.Agent.ID); err != nil {
			return err
		}
		if _, duplicate := seenAgents[user.Agent.ID]; duplicate {
			return errors.New("users must have distinct agent.id values")
		}
		seenAgents[user.Agent.ID] = struct{}{}
		if strings.TrimSpace(user.Agent.Name) == "" ||
			strings.TrimSpace(user.Agent.Capability) == "" ||
			len(user.Agent.Skills) == 0 ||
			strings.TrimSpace(user.Agent.ExecutorRef) == "" {
			return fmt.Errorf(
				"%s.agent requires name, capability, and at least one skill",
				prefix,
			)
		}
		if !tokenEnvironmentName.MatchString(user.Agent.RegistrationKeyEnv) {
			return fmt.Errorf(
				"%s.agent.registration_key_env must be an uppercase environment variable name",
				prefix)
		}
		for skillIndex, skill := range user.Agent.Skills {
			if strings.TrimSpace(skill) == "" {
				return fmt.Errorf(
					"%s.agent.skills[%d] is required", prefix, skillIndex)
			}
		}
		user.trustedID = "oidc:" + c.OIDCIssuer + "#" + user.OIDCSubject
		if err := validateID(prefix+".trusted_subject", user.trustedID); err != nil {
			return fmt.Errorf(
				"%s OIDC issuer and subject produce an invalid Shoal identity: %w",
				prefix, err,
			)
		}
		user.workspaceKey = base64.RawURLEncoding.EncodeToString(
			[]byte(user.WorkspaceID))
	}
	return nil
}

func (c workspaceConfig) validate() error {
	switch {
	case c.RetrievalTopK == 0 || c.RetrievalTopK > workspace.MaxRetrievalTopK:
		return errors.New("workspace.retrieval_top_k is outside the public bound")
	case c.GraphDepth == 0 || c.GraphDepth > workspace.MaxGraphDepth:
		return errors.New("workspace.graph_depth is outside the public bound")
	case c.GraphFanout == 0 || c.GraphFanout > workspace.MaxGraphFanout:
		return errors.New("workspace.graph_fanout is outside the public bound")
	case c.GraphNodes == 0 || c.GraphNodes > workspace.MaxGraphNodes:
		return errors.New("workspace.graph_nodes is outside the public bound")
	case c.OutputBytes == 0 || c.OutputBytes > workspace.MaxOutputBytes:
		return errors.New("workspace.output_bytes is outside the public bound")
	default:
		return nil
	}
}

func validateEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("must be an absolute HTTP(S) origin without a path")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, errors.New("scheme must be http or https")
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
		return nil, errors.New("plain HTTP is allowed only for a loopback host")
	}
	return parsed, nil
}

func validateIssuer(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		strings.Contains(raw, "#") || strings.TrimSpace(raw) != raw {
		return "", errors.New("must be an absolute HTTPS issuer URL")
	}
	return raw, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func validateID(name, value string) error {
	if err := shoal.ValidateRequiredID(name, shoal.ID(value)); err != nil {
		return err
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not contain surrounding whitespace", name)
	}
	return nil
}

func buildScenario(c config) (scenario, error) {
	start, err := time.Parse(time.RFC3339, c.ScenarioStart)
	if err != nil {
		return scenario{}, err
	}
	team := map[string]any{
		"id": c.Team.ID, "name": c.Team.Name,
		"person_ids": []string{c.Users[0].PersonID, c.Users[1].PersonID},
		"agent_ids":  []string{c.Users[0].Agent.ID, c.Users[1].Agent.ID},
	}
	workItems := makeWorkItems(c, start)
	activities := makeActivities(c, start, workItems)
	plans := make([]userPlan, len(c.Users))
	for index, user := range c.Users {
		plans[index].User = user
		person := map[string]any{
			"id": user.PersonID, "display_name": user.DisplayName,
			"team_id": c.Team.ID, "team_role": user.TeamRole,
			"oidc_subject_id":  user.OIDCSubject,
			"shoal_subject_id": user.trustedID,
		}
		agent := map[string]any{
			"id": user.Agent.ID, "name": user.Agent.Name,
			"team_id": c.Team.ID, "owner_person_id": user.PersonID,
			"owner_shoal_subject_id": user.trustedID,
			"mode":                   "simulated", "capability": user.Agent.Capability,
			"skills": append([]string(nil), user.Agent.Skills...),
		}
		files := []fixtureFile{}
		if index == 0 {
			file, err := makeFixture("shoal-demo-01-team.txt", "team", []any{team})
			if err != nil {
				return scenario{}, err
			}
			files = append(files, file)
		}
		personFile, err := makeFixture(
			fmt.Sprintf("shoal-demo-%02d-person.txt", 2+index),
			"person", []any{person})
		if err != nil {
			return scenario{}, err
		}
		agentFile, err := makeFixture(
			fmt.Sprintf("shoal-demo-%02d-agent.txt", 4+index),
			"simulated_agent", []any{agent})
		if err != nil {
			return scenario{}, err
		}
		files = append(files, personFile, agentFile)
		itemRecords := make([]any, 0, len(workItems))
		for itemIndex, item := range workItems {
			if itemIndex%len(c.Users) == index {
				itemRecords = append(itemRecords, item)
			}
		}
		itemFile, err := makeFixture(
			fmt.Sprintf("shoal-demo-%02d-work-items.txt", 6+index),
			"work_item", itemRecords)
		if err != nil {
			return scenario{}, err
		}
		activityRecords := make([]any, 0, len(activities))
		for activityIndex, activity := range activities {
			if activityIndex%len(c.Users) == index {
				activityRecords = append(activityRecords, activity)
			}
		}
		activityFile, err := makeFixture(
			fmt.Sprintf("shoal-demo-%02d-activity.txt", 8+index),
			"activity", activityRecords)
		if err != nil {
			return scenario{}, err
		}
		files = append(files, itemFile, activityFile)
		plans[index].Files = files
	}
	nodes, relations := makeGraph(c, workItems, activities)
	return scenario{Plans: plans, Nodes: nodes, Relations: relations}, nil
}

func graphKey(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func makeGraph(
	c config, workItems, activities []map[string]any,
) ([]webapi.GraphMaterializeNode, []webapi.GraphMaterializeRelation) {
	nodes := []webapi.GraphMaterializeNode{{
		Key: graphKey(c.Team.ID), Kind: "team",
		Properties: shoal.Metadata{"name": c.Team.Name},
	}}
	var relations []webapi.GraphMaterializeRelation
	addMember := func(key, member string) {
		relations = append(relations, webapi.GraphMaterializeRelation{
			Key: graphKey("member-of:" + key), From: graphKey(member),
			To: graphKey(c.Team.ID), Type: "member_of",
		})
	}
	for _, user := range c.Users {
		nodes = append(nodes,
			webapi.GraphMaterializeNode{
				Key: graphKey(user.PersonID), Kind: "person",
				Properties: shoal.Metadata{
					"name": user.DisplayName, "subject_id": user.trustedID,
				},
			},
			webapi.GraphMaterializeNode{
				Key: graphKey(user.Agent.ID), Kind: "agent",
				Properties: shoal.Metadata{
					"name": user.Agent.Name, "agent_id": user.Agent.ID,
				},
			},
		)
		addMember(user.PersonID, user.PersonID)
		addMember(user.Agent.ID, user.Agent.ID)
	}
	for index, item := range workItems {
		id := item["id"].(string)
		nodes = append(nodes, webapi.GraphMaterializeNode{
			Key: graphKey(id), Kind: "work_item",
			Properties: shoal.Metadata{
				"title": item["title"].(string), "status": item["state"].(string),
			},
		})
		addMember(id, id)
		relations = append(relations, webapi.GraphMaterializeRelation{
			Key: graphKey("assigned-to:" + id), From: graphKey(id),
			To: graphKey(item["assignee_person_id"].(string)), Type: "assigned_to",
		})
		if index == 4 {
			relations = append(relations, webapi.GraphMaterializeRelation{
				Key: graphKey("blocked-by:" + id), From: graphKey(id),
				To: graphKey(workItems[2]["id"].(string)), Type: "blocked_by",
			})
		}
	}
	for _, activity := range activities {
		id := activity["id"].(string)
		nodes = append(nodes, webapi.GraphMaterializeNode{
			Key: graphKey(id), Kind: "activity",
			Properties: shoal.Metadata{
				"recorded_at": activity["occurred_at"].(string),
				"actor_id":    activity["actor_id"].(string),
				"operation":   activity["event"].(string),
				"status":      activity["outcome"].(string), "manual": "true",
			},
		})
		addMember(id, id)
	}
	return nodes, relations
}

func makeWorkItems(c config, start time.Time) []map[string]any {
	definitions := []struct {
		title    string
		state    string
		assignee int
	}{
		{"Define shared service boundaries", "completed", 0},
		{"Implement deterministic fixture ingestion", "completed", 1},
		{"Add authorization regression tests", "completed", 0},
		{"Document the operator runbook", "active", 1},
		{"Investigate the review queue bottleneck", "blocked", 0},
		{"Automate the release checklist", "active", 0},
		{"Validate restart durability", "completed", 1},
	}
	items := make([]map[string]any, 0, len(definitions))
	for index, definition := range definitions {
		created := start.AddDate(0, 0, index-3).Add(9 * time.Hour)
		item := map[string]any{
			"id":      fmt.Sprintf("demo-work-%02d", index+1),
			"team_id": c.Team.ID, "title": definition.title,
			"state": definition.state, "priority": 1 + index%3,
			"assignee_person_id": c.Users[definition.assignee].PersonID,
			"created_at":         created.Format(time.RFC3339),
			"evidence_id":        fmt.Sprintf("demo-evidence-work-%02d", index+1),
		}
		if definition.state != "blocked" {
			item["started_at"] = created.Add(24 * time.Hour).Format(time.RFC3339)
		}
		if definition.state == "completed" {
			item["completed_at"] = created.Add(
				time.Duration(2+index%3) * 24 * time.Hour).Format(time.RFC3339)
		}
		if definition.state == "blocked" {
			item["blocked_reason"] = "awaiting bounded review capacity"
		}
		items = append(items, item)
	}
	return items
}

func makeActivities(
	c config,
	start time.Time,
	workItems []map[string]any,
) []map[string]any {
	events := []string{
		"work_item.created", "agent.run.completed", "work_item.started",
		"review.completed", "agent.run.failed", "work_item.completed",
		"work_item.updated",
	}
	activities := make([]map[string]any, 0, activityDays)
	for day := 0; day < activityDays; day++ {
		owner := day % len(c.Users)
		actorKind := "person"
		actorID := c.Users[owner].PersonID
		if (day/len(c.Users))%2 == 1 {
			actorKind = "simulated_agent"
			actorID = c.Users[owner].Agent.ID
		}
		outcome := "success"
		if events[day%len(events)] == "agent.run.failed" {
			outcome = "failure"
		}
		item := workItems[day%len(workItems)]
		activities = append(activities, map[string]any{
			"id":      fmt.Sprintf("demo-activity-%02d", day+1),
			"team_id": c.Team.ID,
			"occurred_at": start.AddDate(0, 0, day).
				Add(time.Duration(9+day%4) * time.Hour).Format(time.RFC3339),
			"actor_kind": actorKind, "actor_id": actorID,
			"event": events[day%len(events)], "outcome": outcome,
			"work_item_id": item["id"],
			"evidence_id":  fmt.Sprintf("demo-evidence-activity-%02d", day+1),
		})
	}
	return activities
}

func makeFixture(name, kind string, records []any) (fixtureFile, error) {
	envelope := struct {
		Schema  string `json:"schema"`
		Kind    string `json:"kind"`
		Records []any  `json:"records"`
	}{
		Schema: scenarioSchema, Kind: kind, Records: records,
	}
	content, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return fixtureFile{}, err
	}
	content = append(content, '\n')
	return fixtureFile{Name: name, Content: content}, nil
}

func (p *provisioner) provision(
	ctx context.Context,
	c config,
	generated scenario,
) (provisionSummary, error) {
	summary := provisionSummary{
		TeamID: c.Team.ID, WorkItems: workItemCount, ActivityDays: activityDays,
		UserSummaries: make([]userSummary, 0, len(generated.Plans)),
	}
	for _, plan := range generated.Plans {
		token := strings.TrimSpace(p.getenv(plan.User.TokenEnv))
		if token == "" {
			return provisionSummary{}, fmt.Errorf(
				"environment variable %s is required", plan.User.TokenEnv)
		}
		if err := p.verifyIdentity(ctx, c.BaseURL, token, plan.User); err != nil {
			return provisionSummary{}, err
		}
		if err := p.putWorkspaceSettings(
			ctx, c.BaseURL, token, plan.User, c.Workspace); err != nil {
			return provisionSummary{}, err
		}
		dispositions, err := p.ingest(
			ctx, c.BaseURL, token, plan.User.workspaceKey, plan.Files)
		if err != nil {
			return provisionSummary{}, err
		}
		summary.UserSummaries = append(summary.UserSummaries, userSummary{
			PersonID: plan.User.PersonID, WorkspaceID: plan.User.WorkspaceID,
			WorkspaceHeader: plan.User.workspaceKey, Files: dispositions,
		})
	}
	owner := generated.Plans[0].User
	token := strings.TrimSpace(p.getenv(owner.TokenEnv))
	snapshot, err := p.currentSnapshot(
		ctx, c.BaseURL, token, owner.workspaceKey)
	if err != nil {
		return provisionSummary{}, err
	}
	graphResult, err := p.materializeGraph(
		ctx, c, token, owner.workspaceKey, snapshot, generated)
	if err != nil {
		return provisionSummary{}, err
	}
	summary.GraphDisposition = string(graphResult.Disposition)
	for _, node := range graphResult.Nodes {
		if node.Key == graphKey(c.Team.ID) {
			summary.TeamNodeID = node.ID
			break
		}
	}
	if summary.TeamNodeID == "" {
		return provisionSummary{}, errors.New("graph response omitted team identity")
	}
	for _, user := range c.Users {
		if err := p.registerAgent(ctx, c, user); err != nil {
			return provisionSummary{}, err
		}
	}
	people, agents, items, err := p.verifyOverview(
		ctx, c, token, owner.workspaceKey, summary.TeamNodeID)
	if err != nil {
		return provisionSummary{}, err
	}
	summary.OverviewPeople, summary.OverviewAgents, summary.OverviewWorkItems =
		people, agents, items
	return summary, nil
}

func (p *provisioner) verifyIdentity(
	ctx context.Context,
	baseURL, token string,
	user userConfig,
) error {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, baseURL+"/api/v1/identity", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("verify %s identity: %w", user.PersonID, err)
	}
	defer response.Body.Close()
	if err := requireStatus(response, http.StatusOK); err != nil {
		return fmt.Errorf("verify %s identity: %w", user.PersonID, err)
	}
	var identity identityResponse
	if err := json.NewDecoder(response.Body).Decode(&identity); err != nil {
		return fmt.Errorf("decode %s identity: %w", user.PersonID, err)
	}
	if !identity.Authenticated || identity.Subject != user.trustedID {
		return fmt.Errorf(
			"%s token resolved to subject %q, want %q",
			user.PersonID, identity.Subject, user.trustedID,
		)
	}
	for _, operation := range []auth.Operation{
		auth.OperationIngest,
		auth.OperationGraphMaterialize,
		auth.OperationWorkspaceSettingsRead,
		auth.OperationWorkspaceSettingsWrite,
		auth.OperationAgentRegister,
		auth.OperationAgentHeartbeat,
		auth.OperationAgentResolve,
		auth.OperationTeamOverviewRead,
	} {
		if !slices.Contains(identity.Operations, string(operation)) {
			return fmt.Errorf(
				"%s token is missing required operation %q",
				user.PersonID, operation,
			)
		}
	}
	return nil
}

func (p *provisioner) currentSnapshot(
	ctx context.Context, baseURL, token, workspaceID string,
) (webapi.Snapshot, error) {
	var response struct {
		Snapshot webapi.Snapshot `json:"snapshot"`
	}
	err := p.doJSON(ctx, http.MethodPost, baseURL+"/api/v1/documents",
		token, workspaceID, map[string]any{
			"page": map[string]any{"limit": 1},
		}, &response, http.StatusOK)
	return response.Snapshot, err
}

func (p *provisioner) materializeGraph(
	ctx context.Context, c config, token, workspaceID string,
	snapshot webapi.Snapshot, generated scenario,
) (webapi.GraphMaterializeResponse, error) {
	request := webapi.GraphMaterializeRequest{
		Namespace:  graphKey(c.Graph.Namespace),
		SourceID:   graphKey(c.Graph.SourceID),
		PolicyID:   graphKey(c.Graph.PolicyID),
		MutationID: graphKey("demo-graph-v1:" + c.Graph.Namespace),
		Snapshot:   snapshot, Nodes: generated.Nodes, Relations: generated.Relations,
	}
	var response webapi.GraphMaterializeResponse
	err := p.doJSON(ctx, http.MethodPost,
		c.BaseURL+"/api/v1/graph/materialize", token, workspaceID,
		request, &response, http.StatusOK)
	if err != nil {
		return webapi.GraphMaterializeResponse{}, fmt.Errorf(
			"materialize demo graph: %w", err)
	}
	return response, nil
}

func (p *provisioner) registerAgent(
	ctx context.Context, c config, user userConfig,
) error {
	token := strings.TrimSpace(p.getenv(user.TokenEnv))
	key := strings.TrimSpace(p.getenv(user.Agent.RegistrationKeyEnv))
	if key == "" {
		return fmt.Errorf("environment variable %s is required",
			user.Agent.RegistrationKeyEnv)
	}
	now := time.Now().UTC()
	contextValue := map[string]any{
		"request_id":  graphKey("demo-agent-request:" + user.Agent.ID),
		"reason_code": "demo_seed", "reason_detail": "refresh simulated demo agent",
		"deadline": now.Add(30 * time.Second),
	}
	agentPath := c.BaseURL + "/api/v1/fleet/agents/" +
		graphKey(user.Agent.ID)
	var existing fleetDescriptorResponse
	found, err := p.resolveAgent(
		ctx, agentPath+"/resolve", token, contextValue, &existing)
	if err != nil {
		return fmt.Errorf("resolve agent %s: %w", user.Agent.ID, err)
	}
	lease := now.Add(23 * time.Hour)
	if found {
		var refreshed fleetDescriptorResponse
		return p.doJSON(ctx, http.MethodPost, agentPath+"/heartbeat",
			token, "", map[string]any{
				"context": contextValue, "registration_key": graphKey(key),
				"expected_generation": existing.Generation,
				"lease_expires_at":    lease,
			}, &refreshed, http.StatusOK)
	}

	var registered fleetDescriptorResponse
	capabilities := []map[string]any{{
		"name": user.Agent.Capability,
		"actions": []map[string]any{{
			"name":          user.Agent.Skills[0],
			"input_schema":  map[string]any{"type": "object"},
			"output_schema": map[string]any{"type": "object"},
		}},
	}}
	return p.doJSON(ctx, http.MethodPost, c.BaseURL+"/api/v1/fleet/agents",
		token, "", map[string]any{
			"context": contextValue, "registration_key": graphKey(key),
			"expected_generation": 0,
			"descriptor": map[string]any{
				"id":                   graphKey(user.Agent.ID),
				"authorization_domain": []byte(c.Graph.AuthorizationDomain),
				"scopes": []map[string]any{{
					"source_id": []byte(c.Graph.SourceID),
					"policy_id": []byte(c.Graph.PolicyID),
				}},
				"executor_ref": user.Agent.ExecutorRef,
				"capabilities": capabilities, "lease_expires_at": lease,
			},
		}, &registered, http.StatusCreated)
}

func (p *provisioner) resolveAgent(
	ctx context.Context, endpoint, token string, input any,
	output *fleetDescriptorResponse,
) (bool, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err := requireStatus(response, http.StatusOK); err != nil {
		return false, err
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return false, err
	}
	return true, nil
}

func (p *provisioner) verifyOverview(
	ctx context.Context, c config, token, workspaceID, encodedTeamID string,
) (int, int, int, error) {
	teamBytes, err := base64.RawURLEncoding.DecodeString(encodedTeamID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("decode team node ID: %w", err)
	}
	var response struct {
		People     []json.RawMessage `json:"people"`
		Agents     []json.RawMessage `json:"agents"`
		WorkItems  []json.RawMessage `json:"work_items"`
		Activities []json.RawMessage `json:"activities"`
	}
	err = p.doJSON(ctx, http.MethodPost, c.BaseURL+"/api/v1/team/overview",
		token, workspaceID, map[string]any{
			"team_id": string(teamBytes), "source_id": []byte(c.Graph.SourceID),
			"policy_id": []byte(c.Graph.PolicyID), "history_days": activityDays,
			"limit": 100,
		}, &response, http.StatusOK)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("verify team overview: %w", err)
	}
	if len(response.People) != 2 || len(response.Agents) != 2 ||
		len(response.WorkItems) != workItemCount ||
		len(response.Activities) != activityDays {
		return 0, 0, 0, fmt.Errorf(
			"team overview mismatch: people=%d agents=%d work_items=%d activities=%d",
			len(response.People), len(response.Agents), len(response.WorkItems),
			len(response.Activities))
	}
	return len(response.People), len(response.Agents), len(response.WorkItems), nil
}

func (p *provisioner) doJSON(
	ctx context.Context, method, endpoint, token, workspaceID string,
	input, output any, want int,
) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if workspaceID != "" {
		request.Header.Set("X-Shoal-Workspace-Request", "1")
		request.Header.Set(webapi.WorkspaceIDHeader, workspaceID)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := requireStatus(response, want); err != nil {
		return err
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(output)
}

func (p *provisioner) putWorkspaceSettings(
	ctx context.Context,
	baseURL, token string,
	user userConfig,
	settings workspaceConfig,
) error {
	payload := struct {
		ExpectedRevision uint64 `json:"expected_revision"`
		MutationID       string `json:"mutation_id"`
		Settings         struct {
			Budgets struct {
				RetrievalTopK uint32 `json:"retrieval_top_k"`
				GraphDepth    uint32 `json:"graph_depth"`
				GraphFanout   uint32 `json:"graph_fanout"`
				GraphNodes    uint32 `json:"graph_nodes"`
				OutputBytes   uint64 `json:"output_bytes"`
			} `json:"budgets"`
		} `json:"settings"`
	}{
		ExpectedRevision: 0,
		MutationID:       workspaceMutationID(user.WorkspaceID),
	}
	payload.Settings.Budgets.RetrievalTopK = settings.RetrievalTopK
	payload.Settings.Budgets.GraphDepth = settings.GraphDepth
	payload.Settings.Budgets.GraphFanout = settings.GraphFanout
	payload.Settings.Budgets.GraphNodes = settings.GraphNodes
	payload.Settings.Budgets.OutputBytes = settings.OutputBytes
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPut,
		baseURL+"/api/v1/workspaces/"+user.workspaceKey+"/settings",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("provision %s workspace settings: %w", user.PersonID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK &&
		response.StatusCode != http.StatusCreated {
		return fmt.Errorf(
			"provision %s workspace settings: %w",
			user.PersonID, requireStatus(response, http.StatusOK),
		)
	}
	return nil
}

func workspaceMutationID(workspaceID string) string {
	digest := sha256.Sum256([]byte("shoal-demo-settings-v1:" + workspaceID))
	return base64.RawURLEncoding.EncodeToString(
		[]byte("demo-settings-" + hex.EncodeToString(digest[:])),
	)
}

func (p *provisioner) ingest(
	ctx context.Context,
	baseURL, token, workspaceID string,
	files []fixtureFile,
) (map[string]string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary("shoal-demo-seed-v1-boundary"); err != nil {
		return nil, err
	}
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.Name)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(file.Content); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, baseURL+"/api/v1/ingest", &body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	request.Header.Set(webapi.WorkspaceIDHeader, workspaceID)
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("ingest workspace fixtures: %w", err)
	}
	defer response.Body.Close()
	if err := requireStatus(response, http.StatusOK); err != nil {
		return nil, fmt.Errorf("ingest workspace fixtures: %w", err)
	}
	var decoded ingestResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode ingest response: %w", err)
	}
	if len(decoded.Files) != len(files) {
		return nil, fmt.Errorf(
			"ingest returned %d files, want %d", len(decoded.Files), len(files))
	}
	expected := make(map[string]struct{}, len(files))
	for _, file := range files {
		expected[file.Name] = struct{}{}
	}
	dispositions := make(map[string]string, len(files))
	for _, file := range decoded.Files {
		if _, ok := expected[file.Name]; !ok {
			return nil, fmt.Errorf(
				"ingest returned unexpected file %q", file.Name)
		}
		switch file.Disposition {
		case "applied", "unchanged":
		case "historical":
			// The fixture matches an earlier revision, so the server left a
			// newer one current and the seed did not take effect (#674).
			return nil, fmt.Errorf(
				"ingest left %s at a newer revision: the fixture matches an "+
					"earlier, non-current revision and was not made current",
				file.Name,
			)
		default:
			return nil, fmt.Errorf(
				"ingest returned invalid disposition %q for %s",
				file.Disposition, file.Name,
			)
		}
		dispositions[file.Name] = file.Disposition
	}
	return dispositions, nil
}

func requireStatus(response *http.Response, want int) error {
	if response.StatusCode == want {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = response.Status
	}
	return fmt.Errorf("HTTP %d: %s", response.StatusCode, message)
}
