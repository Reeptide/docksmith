package container

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Names are adjective_noun, so containers can be referred to by something
// pronounceable instead of a hex prefix.
var (
	adjectives = []string{
		"amber", "bold", "brave", "calm", "clever", "cosmic", "crimson", "dawn",
		"eager", "elder", "fierce", "gentle", "golden", "hidden", "hollow",
		"ivory", "jolly", "keen", "lucid", "mellow", "noble", "quiet", "rapid",
		"rustic", "silent", "solar", "stark", "swift", "tidal", "vivid", "wry",
	}
	nouns = []string{
		"anvil", "beacon", "bellows", "cinder", "crucible", "ember", "forge",
		"furnace", "gasket", "girder", "hammer", "ingot", "kiln", "lathe",
		"mandrel", "mortar", "piston", "quarry", "rivet", "sledge", "smelter",
		"spindle", "tinder", "tongs", "trestle", "turbine", "vise", "welder",
	}
)

// GenerateName returns an unused adjective_noun name.
func GenerateName(stateRoot string) string {
	taken := make(map[string]bool)
	if existing, err := List(stateRoot); err == nil {
		for _, r := range existing {
			taken[r.Name] = true
		}
	}

	for attempt := 0; attempt < 100; attempt++ {
		name := randomWord(adjectives) + "_" + randomWord(nouns)
		if !taken[name] {
			return name
		}
	}
	// Every combination in use is implausible, but a caller still needs a name.
	return fmt.Sprintf("%s_%s_%d", randomWord(adjectives), randomWord(nouns), time.Now().UnixNano()%1000)
}

func randomWord(words []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return words[0]
	}
	return words[n.Int64()]
}

// Status renders the STATUS column: how long it has been up, or how it ended.
func (r *Record) Status() string {
	switch r.State {
	case StateRunning:
		if t, err := time.Parse(time.RFC3339, r.StartedAt); err == nil {
			return "Up " + humanDuration(time.Since(t))
		}
		return "Up"
	case StateExited:
		suffix := ""
		if t, err := time.Parse(time.RFC3339, r.FinishedAt); err == nil {
			suffix = " " + humanDuration(time.Since(t)) + " ago"
		}
		return fmt.Sprintf("Exited (%d)%s", r.ExitCode, suffix)
	default:
		return "Created"
	}
}

// CreatedAgo renders the CREATED column.
func (r *Record) CreatedAgo() string {
	t, err := time.Parse(time.RFC3339, r.Created)
	if err != nil {
		return r.Created
	}
	return humanDuration(time.Since(t)) + " ago"
}

// CommandString renders the COMMAND column, truncated to stay in its column.
func (r *Record) CommandString() string {
	s := strings.Join(r.Command, " ")
	const max = 22
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

func humanDuration(d time.Duration) string {
	switch {
	case d < 2*time.Second:
		return "1 second"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < 2*time.Minute:
		return "1 minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 2*time.Hour:
		return "1 hour"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d < 48*time.Hour:
		return "1 day"
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
