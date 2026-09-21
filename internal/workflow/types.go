package workflow

import (
	"encoding/json"
	"time"
)

const maxHistory = 256
const maxTransitions = 32

type Definition struct {
	Version int                `json:"version"`
	ID      string             `json:"id"`
	Title   string             `json:"title"`
	Entry   string             `json:"entry"`
	Facts   map[string]any     `json:"facts"`
	Actions map[string]*Action `json:"actions"`
}

type Action struct {
	ID           string    `json:"-"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Terminal     bool      `json:"terminal"`
	AllowedTools *[]string `json:"allowedTools"`
	Outcomes     []Outcome `json:"outcomes"`
	Routes       []Route   `json:"routes"`
	Gates        []Gate    `json:"gates"`
	Requires     any       `json:"requires"`
	CompleteWhen any       `json:"completeWhen"`
}

type Outcome struct {
	ID   string         `json:"id"`
	When any            `json:"when"`
	Set  map[string]any `json:"set"`
}

type Route struct {
	To   string `json:"to"`
	When any    `json:"when"`
}

type Gate struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Fact        string `json:"fact"`
	Equals      any    `json:"equals"`
	HasEquals   bool   `json:"-"`
	Condition   any    `json:"condition"`
}

func (g *Gate) UnmarshalJSON(data []byte) error {
	type alias Gate
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*g = Gate(decoded)
	_, g.HasEquals = raw["equals"]
	return nil
}

type State struct {
	CurrentAction string           `json:"currentAction"`
	Facts         map[string]any   `json:"facts"`
	FactSources   map[string]any   `json:"factSources"`
	History       []map[string]any `json:"history"`
	Status        string           `json:"status"`
}

type Snapshot struct {
	Version       int              `json:"version"`
	Definition    *Definition      `json:"definition"`
	CurrentAction string           `json:"currentAction"`
	Facts         map[string]any   `json:"facts"`
	FactSources   map[string]any   `json:"factSources"`
	History       []map[string]any `json:"history"`
	Status        string           `json:"status"`
}

type GateSummary struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type Summary struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	CurrentAction string         `json:"currentAction"`
	CurrentTitle  string         `json:"currentTitle"`
	Terminal      bool           `json:"terminal"`
	Status        string         `json:"status"`
	AllowedTools  *[]string      `json:"allowedTools"`
	CanFinish     bool           `json:"canFinish"`
	PendingGates  []GateSummary  `json:"pendingGates"`
	Facts         map[string]any `json:"facts"`
}

type Transition struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type ChangedFact struct {
	Path     string `json:"path"`
	Previous any    `json:"previous"`
	Value    any    `json:"value"`
}

type ObserveResult struct {
	ChangedFacts  []ChangedFact `json:"changedFacts"`
	Transition    *Transition   `json:"transition,omitempty"`
	BeforeAction  string        `json:"beforeAction"`
	CurrentAction string        `json:"currentAction"`
	Status        string        `json:"status"`
}

type ToolObservation struct {
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Result map[string]any `json:"result"`
	Step   any            `json:"step"`
}

type EvalContext struct {
	Facts      map[string]any
	Tool       string
	Args       map[string]any
	Result     map[string]any
	ResultData map[string]any
}

type Runtime struct {
	Definition    *Definition
	CurrentAction string
	Facts         map[string]any
	FactSources   map[string]any
	History       []map[string]any
	Status        string
}

var nowUTC = func() time.Time { return time.Now().UTC() }
