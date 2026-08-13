package freezewatch

import (
	"encoding/json"
	"fmt"
)

// Payload is the JSON body posted to the alert webhook.
//
// Shaped for a generic webhook rather than one vendor's schema: `text` renders
// in Slack and Discord, and the structured fields survive for anything that
// parses the body.
type Payload struct {
	Text     string        `json:"text"`
	Subject  string        `json:"subject"`
	Critical int           `json:"critical"`
	Warning  int           `json:"warning"`
	Chain    string        `json:"chain"`
	At       uint64        `json:"observedAt"`
	Alerts   []AlertDetail `json:"alerts"`
}

// AlertDetail is one alert in the payload.
type AlertDetail struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Subject  string `json:"subject,omitempty"`
	Message  string `json:"message"`
}

// BuildPayload renders alerts into a webhook body.
func BuildPayload(alerts []Alert, chain string, observedAt uint64) Payload {
	s := Summarize(alerts)

	details := make([]AlertDetail, len(alerts))
	lines := make([]string, 0, len(alerts)+1)
	lines = append(lines, fmt.Sprintf("%s (%s, block time %d)", s.Subject(), chain, observedAt))
	for i, a := range alerts {
		details[i] = AlertDetail{
			Kind:     string(a.Kind),
			Severity: string(a.Severity),
			Subject:  a.Subject,
			Message:  a.Message,
		}
		lines = append(lines, "• "+a.String())
	}

	text := lines[0]
	for _, l := range lines[1:] {
		text += "\n" + l
	}

	return Payload{
		Text:     text,
		Subject:  s.Subject(),
		Critical: s.Critical,
		Warning:  s.Warning,
		Chain:    chain,
		At:       observedAt,
		Alerts:   details,
	}
}

// MarshalPayload renders the body bytes.
func MarshalPayload(p Payload) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("freezewatch: encoding alert payload: %w", err)
	}
	return b, nil
}

// ShouldNotify decides whether this tick warrants a webhook call.
//
// Quiet ticks send nothing. W4 runs every ~10 minutes, and a channel that
// receives "all clear" 144 times a day is a channel nobody reads — which
// defeats the point of having it.
//
// The tradeoff is that silence becomes ambiguous between "healthy" and "W4 is
// down". That is deliberately left to the CRE platform's own execution
// monitoring rather than solved with heartbeat spam here.
func ShouldNotify(alerts []Alert) bool {
	return len(alerts) > 0
}
