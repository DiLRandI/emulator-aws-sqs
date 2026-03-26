package model

// Registry describes the SQS API surface loaded from the official service model.
type Registry struct {
	Metadata   Metadata             `json:"metadata"`
	Operations map[string]Operation `json:"operations"`
	Shapes     map[string]Shape     `json:"shapes"`
	Pagination map[string]Paginator `json:"pagination"`
}

type Metadata struct {
	APIVersion     string `json:"apiVersion"`
	EndpointPrefix string `json:"endpointPrefix"`
	JSONVersion    string `json:"jsonVersion"`
	Protocol       string `json:"protocol"`
	TargetPrefix   string `json:"targetPrefix"`
	QueryNamespace string `json:"queryNamespace"`
	ServiceID      string `json:"serviceId"`
}

type Operation struct {
	Name        string   `json:"name"`
	InputShape  string   `json:"inputShape,omitempty"`
	OutputShape string   `json:"outputShape,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

type Paginator struct {
	InputToken  string `json:"inputToken,omitempty"`
	OutputToken string `json:"outputToken,omitempty"`
	LimitKey    string `json:"limitKey,omitempty"`
	ResultKey   string `json:"resultKey,omitempty"`
}

type Shape struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Enum        []string          `json:"enum,omitempty"`
	Required    []string          `json:"required,omitempty"`
	MemberOrder []string          `json:"memberOrder,omitempty"`
	Members     map[string]Member `json:"members,omitempty"`
	Key         *ShapeRef         `json:"key,omitempty"`
	Value       *ShapeRef         `json:"value,omitempty"`
	Member      *ShapeRef         `json:"member,omitempty"`
	Flattened   bool              `json:"flattened,omitempty"`
	Exception   bool              `json:"exception,omitempty"`
}

type ShapeRef struct {
	Shape        string `json:"shape"`
	LocationName string `json:"locationName,omitempty"`
	Flattened    bool   `json:"flattened,omitempty"`
}

type Member struct {
	Shape        string `json:"shape"`
	LocationName string `json:"locationName,omitempty"`
	Flattened    bool   `json:"flattened,omitempty"`
}
