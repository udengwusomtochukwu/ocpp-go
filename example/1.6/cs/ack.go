package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// hyde/lab: alert acknowledgement. A critical Teams alert repeats until someone
// clicks "Acknowledge" — which hits this endpoint (GET, so a link works),
// creates a Grafana silence that stops the reminders, records the ack, and
// posts back to Teams so everyone can see it was handled. The acker needs no
// Grafana login: this endpoint holds the admin credential server-side.

const (
	envGrafanaSilenceURL = "GRAFANA_SILENCE_URL" // .../api/alertmanager/grafana/api/v2/silences
	envGrafanaAuth       = "GRAFANA_AUTH"        // "user:password" for basic auth
	ackSilenceHours      = 4
)

func handleAck(w http.ResponseWriter, r *http.Request) {
	alertname := r.URL.Query().Get("alertname")
	by := strings.TrimSpace(r.URL.Query().Get("by"))
	if by == "" {
		by = "a team member"
	}
	if alertname == "" {
		writeAckPage(w, "Missing alert reference — nothing to acknowledge.", false)
		return
	}
	sid, err := createSilence(alertname, by)
	if err != nil {
		log.Errorf("ack: silence failed for %q: %v", alertname, err)
		writeAckPage(w, "Noted, but the reminders could not be paused automatically. Please let the team know you're on it.", false)
		return
	}
	log.Infof("ack: %q acknowledged by %s (silence %s, %dh)", alertname, by, sid, ackSilenceHours)
	postAckToTeams(alertname, by)
	writeAckPage(w, fmt.Sprintf("Acknowledged. Reminders for this alert are paused for %d hours. Thank you, %s.", ackSilenceHours, by), true)
}

func createSilence(alertname, by string) (string, error) {
	url := os.Getenv(envGrafanaSilenceURL)
	auth := os.Getenv(envGrafanaAuth)
	if url == "" || auth == "" {
		return "", fmt.Errorf("silence endpoint not configured")
	}
	now := time.Now().UTC()
	payload := map[string]any{
		"matchers":  []map[string]any{{"name": "alertname", "value": alertname, "isRegex": false, "isEqual": true}},
		"startsAt":  now.Format("2006-01-02T15:04:05.000Z"),
		"endsAt":    now.Add(ackSilenceHours * time.Hour).Format("2006-01-02T15:04:05.000Z"),
		"createdBy": "teams-ack:" + by,
		"comment":   "Acknowledged via Teams by " + by,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if u, p, ok := strings.Cut(auth, ":"); ok {
		req.SetBasicAuth(u, p)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("grafana returned %d", resp.StatusCode)
	}
	var out struct {
		SilenceID string `json:"silenceID"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.SilenceID, nil
}

// postAckToTeams posts a short confirmation back to the channel and the chat so
// everyone can see the alert was reacted to. Best-effort, non-blocking.
func postAckToTeams(alertname, by string) {
	text := fmt.Sprintf("✅ **Acknowledged** by %s — reminders paused for %dh.\n\n_%s_", by, ackSilenceHours, alertname)
	for _, k := range []string{"TEAMS_WEBHOOK_URL", "TEAMS_CHAT_WEBHOOK_URL"} {
		if url := os.Getenv(k); url != "" {
			go postTeamsCard(url, text)
		}
	}
}

func postTeamsCard(url, text string) {
	card := map[string]any{"type": "message", "attachments": []map[string]any{{
		"contentType": "application/vnd.microsoft.card.adaptive",
		"content": map[string]any{
			"type": "AdaptiveCard", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "version": "1.4",
			"body": []map[string]any{{"type": "TextBlock", "text": text, "wrap": true}},
		}}}}
	b, _ := json.Marshal(card)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); err == nil {
		resp.Body.Close()
	}
}

func writeAckPage(w http.ResponseWriter, msg string, ok bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	color, icon := "#3DDC5F", "✅"
	if !ok {
		color, icon = "#E5484D", "⚠️"
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Acknowledged · HYDE</title></head><body style="font-family:system-ui,'Segoe UI',sans-serif;background:#0c1210;color:#d7e0dc;display:grid;place-items:center;min-height:100vh;margin:0"><div style="text-align:center;max-width:34rem;padding:2rem"><div style="font-size:3.2rem">%s</div><h1 style="color:%s;font-weight:700;letter-spacing:-.01em">%s</h1><p style="color:#8ba099;font-size:.85rem;letter-spacing:.14em;text-transform:uppercase">HYDE OCPP · PEAK250 alerting</p></div></body></html>`, icon, color, msg)
}
