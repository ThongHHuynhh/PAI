package statefile

// Route and Verification are append-only inference metadata, not instructions.
const (
	Route        Kind = "route"
	Verification Kind = "verification"
)

// Routing is explicit permission to send this session to the listed accounts
// and models. Provider names refer to configured endpoints, including aliases.
// No task labels or permissions are inferred from user content.
type Routing struct {
	AllowedModels    []string      `json:"allowed_models"`
	Difficulty       int           `json:"difficulty"`  // 1..3, supplied by the caller
	Consequence      int           `json:"consequence"` // 1..3, supplied by the caller
	Rules            []RouteRule   `json:"rules"`
	Levels           []RouteLevel  `json:"levels"`             // increasing capability, chosen by the operator
	FailuresPerLevel int           `json:"failures_per_level"` // observed nonzero tool exits; 0 disables
	StallSteps       int           `json:"stall_steps"`        // steps without a final answer; 0 disables
	MaxSteps         int           `json:"max_steps"`          // hard per-user-turn inference budget
	Verify           *VerifyPolicy `json:"verify,omitempty"`
}

type RouteRule struct {
	Difficulty  int `json:"difficulty"`
	Consequence int `json:"consequence"`
	Level       int `json:"level"` // zero-based; maximum matching level wins
}

type RouteLevel struct {
	Models []string `json:"models"`           // ordered approved accounts; keep first healthy one
	Effort string   `json:"effort,omitempty"` // catalog setting, independent of model
}

type VerifyPolicy struct {
	Model          string `json:"model"`
	Effort         string `json:"effort,omitempty"`
	MinConsequence int    `json:"min_consequence"`
}
