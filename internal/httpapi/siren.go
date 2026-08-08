package httpapi

// Siren entity types.
//
// Siren (https://github.com/kevinswiber/siren) is used rather than HAL because
// HAL describes links but not *actions*: it can say "here is the fans
// resource", but not "you may POST here, with this field, to switch it off".
// Actions with parameters are the entire point of this API being
// self-describing, so the representation has to carry them.
//
// Only the parts of Siren this API actually emits are modelled. A client
// walking these structures needs no Siren library; it needs a JSON parser and
// the rules in docs/CLIENT.md.

// Entity is a Siren entity: the representation of one resource.
type Entity struct {
	// Class is the entity's type, e.g. ["status"] or ["host","f"]. Clients
	// may switch on it, but should treat unknown classes as displayable
	// rather than as an error.
	Class []string `json:"class,omitempty"`
	// Rel is how an embedded entity relates to its parent. Only set on
	// entities inside Entities.
	Rel []string `json:"rel,omitempty"`
	// Properties are this entity's own data.
	Properties map[string]any `json:"properties,omitempty"`
	// Entities are embedded sub-entities, e.g. one per host.
	Entities []Entity `json:"entities,omitempty"`
	// Links are navigation: where else a client may go from here.
	Links []Link `json:"links,omitempty"`
	// Actions are state changes available *right now*. An action that is not
	// currently possible is omitted entirely rather than flagged, so a client
	// renders exactly what it is offered.
	Actions []Action `json:"actions,omitempty"`
	// Title is human-readable text for the entity as a whole.
	Title string `json:"title,omitempty"`
}

// Link is a navigable relation.
type Link struct {
	Rel   []string `json:"rel"`
	Href  string   `json:"href"`
	Title string   `json:"title,omitempty"`
}

// Action is a state change a client may perform.
type Action struct {
	// Name is the stable identifier. Clients match on this, never on Href.
	Name string `json:"name"`
	// Title is human-readable text for a button or menu entry.
	Title string `json:"title,omitempty"`
	// Method is the HTTP method to use.
	Method string `json:"method"`
	// Href is where to send it. It may change between releases; that is why
	// clients are told to follow Name and read Href rather than build it.
	Href string `json:"href"`
	// Type is the request content type, when the action takes fields.
	Type string `json:"type,omitempty"`
	// Fields are the parameters this action accepts. A field appears only
	// when it is meaningful -- the fans-off `force` field, for instance, is
	// present only while a host is still up.
	Fields []Field `json:"fields,omitempty"`
}

// Field is one parameter of an Action.
type Field struct {
	Name string `json:"name"`
	// Type follows Siren's HTML5-input vocabulary; this API uses "checkbox"
	// for booleans. Clients should render an unknown type as a text input
	// rather than refusing the action.
	Type string `json:"type"`
	// Value is the default.
	Value any `json:"value,omitempty"`
	// Title explains what the field means. Clients should show this text
	// rather than hard-coding their own wording for a known field name.
	Title string `json:"title,omitempty"`
	// Required marks a field the server will reject the action without.
	Required bool `json:"required,omitempty"`
}

// sirenMediaType is the content type for every entity response.
const sirenMediaType = "application/vnd.siren+json"
