package flywheel

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

const (
	labelMaxTokens   = 24
	labelExamplesMax = 5
)

// Labeler names clusters with the cheap title model through the same Provider
// the service uses. It costs money: EstimateLabel first.
type Labeler struct {
	Provider assistant.Provider
	Model    string
}

func labelPrompt(c *Cluster) string {
	var b strings.Builder
	b.WriteString("These are visitor messages (personal data already removed) that Medi, the Media Manipulator assistant, answered badly. ")
	b.WriteString("Name what they have in common as a label of at most 6 words, in English. Reply with the label only.\n\n")
	fmt.Fprintf(&b, "Surface: %s\nSignals: %s\n", c.Surface, signalList(c))
	for i, f := range c.Turns {
		if i == labelExamplesMax {
			break
		}
		fmt.Fprintf(&b, "- %s\n", Example(f.Turn.UserText))
	}
	return b.String()
}

// EstimateLabel prices a labelling pass over clusters: a typical figure from
// the catalog prices (4 characters per token), plus the output cap.
func EstimateLabel(model string, clusters []*Cluster) (calls int, usd float64) {
	for _, c := range clusters {
		in := len(labelPrompt(c))/4 + 1
		usd += assistant.EstimateUSD(model, assistant.Usage{InputTokens: in, OutputTokens: labelMaxTokens})
		calls++
	}
	return calls, usd
}

// LabelAll sets Cluster.Label for each cluster and returns the cost reported by
// the provider (catalog-price estimate when it reports none). A failed call
// leaves that label empty.
func (l *Labeler) LabelAll(ctx context.Context, clusters []*Cluster) (float64, error) {
	if l.Provider == nil {
		return 0, fmt.Errorf("flywheel: no provider")
	}
	total := 0.0
	for _, c := range clusters {
		cost, label, err := l.one(ctx, c)
		total += cost
		if err != nil {
			return total, err
		}
		c.Label = label
	}
	return total, nil
}

func (l *Labeler) one(ctx context.Context, c *Cluster) (float64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	st, err := l.Provider.Stream(ctx, assistant.Request{Model: l.Model, MaxTokens: labelMaxTokens,
		Messages: []assistant.Message{{Role: "user", Text: labelPrompt(c)}}})
	if err != nil {
		return 0, "", err
	}
	defer st.Close()
	var b strings.Builder
	var u *assistant.Usage
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, "", err
		}
		switch ev.Kind {
		case assistant.EventDelta:
			b.WriteString(ev.Text)
		case assistant.EventUsage:
			u = ev.Usage
		case assistant.EventDone:
		}
		if ev.Kind == assistant.EventDone {
			break
		}
	}
	cost := 0.0
	if u != nil {
		cost = u.CostUSD
		if !u.Reported {
			cost = assistant.EstimateUSD(l.Model, *u)
		}
	}
	label := strings.Trim(strings.TrimSpace(strings.SplitN(b.String(), "\n", 2)[0]), `"'.`)
	return cost, Truncate(Scrub(label), 60), nil
}
