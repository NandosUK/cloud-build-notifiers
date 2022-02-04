// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/cloud-build-notifiers/lib/notifiers"
	log "github.com/golang/glog"
	cbpb "google.golang.org/genproto/googleapis/devtools/cloudbuild/v1"
)

type pagerDutyEvent struct {
	Payload     pagerDutyPayload `json:"payload"`
	RoutingKey  string           `json:"routing_key"`
	EventAction string           `json:"event_action"`
	Links       []pagerDutyLink  `json:"links"`
}

type pagerDutyPayload struct {
	Summary  string `json:"summary"`
	Severity string `json:"severity"`
	Source   string `json:"source"`
}

type pagerDutyLink struct {
	Href string `json:"href"`
	Text string `json:"text"`
}

type pagerDutyIncidentNotifier struct {
	filter notifiers.EventFilter

	incidentTitle  string
	integrationKey string
}

const (
	pagerDutyIntegrationKeyName  = "integrationKey"
	pagerDutyEventsAPIV2Endpoint = "https://events.pagerduty.com/v2/enqueue"
)

func main() {
	if err := notifiers.Main(new(pagerDutyIncidentNotifier)); err != nil {
		log.Fatalf("fatal error: %v", err)
	}
}

func getSecret(ctx context.Context, cfg *notifiers.Config, sg notifiers.SecretGetter, secretName string) (string, error) {
	secretRef, err := notifiers.GetSecretRef(cfg.Spec.Notification.Delivery, secretName)
	if err != nil {
		return "", fmt.Errorf("failed to get Secret ref from delivery config (%v) field: %q: %w", cfg.Spec.Notification.Delivery, secretName, err)
	}

	secretResource, err := notifiers.FindSecretResourceName(cfg.Spec.Secrets, secretRef)
	if err != nil {
		return "", fmt.Errorf("failed to find Secret for ref %q: %w", secretRef, err)
	}

	secret, err := sg.GetSecret(ctx, secretResource)
	if err != nil {
		return "", fmt.Errorf("failed to get %s secret: %w", secretName, err)
	}

	return secret, nil
}

func (h *pagerDutyIncidentNotifier) SetUp(ctx context.Context, cfg *notifiers.Config, sg notifiers.SecretGetter, _ notifiers.BindingResolver) error {
	prd, err := notifiers.MakeCELPredicate(cfg.Spec.Notification.Filter)
	if err != nil {
		return fmt.Errorf("failed to create CELPredicate: %w", err)
	}
	h.filter = prd

	incidentTitle, ok := cfg.Spec.Notification.Delivery["incidentTitle"].(string)
	if !ok {
		return fmt.Errorf("expected delivery config %v to have string field `incidentTitle`", cfg.Spec.Notification.Delivery)
	}
	h.incidentTitle = incidentTitle

	key, err := getSecret(ctx, cfg, sg, pagerDutyIntegrationKeyName)
	if err != nil {
		return err
	}
	h.integrationKey = key

	return nil
}

func (h *pagerDutyIncidentNotifier) SendNotification(ctx context.Context, build *cbpb.Build) error {
	if !h.filter.Apply(ctx, build) {
		log.V(2).Infof("not reporting an incident for event (build id = %s, status = %v)", build.Id, build.Status)
		return nil
	}

	log.Infof("reporting an incident for event (build id = %s, status = %s)", build.Id, build.Status)

	requestBody, err := json.Marshal(
		pagerDutyEvent{
			Payload: pagerDutyPayload{
				Summary:  h.incidentTitle,
				Severity: "critical",
				Source:   fmt.Sprintf("https://console.cloud.google.com/cloud-build/builds/%s?project=%s", build.Id, build.ProjectId),
			},
			EventAction: "trigger",
			RoutingKey:  h.integrationKey,
			Links: []pagerDutyLink{
				{
					Href: build.LogUrl,
					Text: "Failing build logs",
				},
			},
		},
	)

	if err != nil {
		fmt.Println(err)
	}

	log.Infof("incident request body: %v", string(requestBody))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pagerDutyEventsAPIV2Endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return fmt.Errorf("failed to create a new HTTP request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GCB-Notifier/0.1 (http)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		log.Warningf("got a non-OK response status %q (%d) from %q", resp.Status, resp.StatusCode, pagerDutyEventsAPIV2Endpoint)
	}

	log.V(2).Infoln("send HTTP request successfully")
	return nil
}
