package contract

import (
	"net/url"
	"strings"
)

// Request is the parsed request: the facts serve() parsed out of the CGI
// environment, handed to every handler in the same shape.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Form   url.Values
	APIKey string
}

// BoolField reads a checkbox field from the query string or the form body.
func (r Request) BoolField(name string) bool {
	for _, v := range []string{r.Form.Get(name), r.Query.Get(name)} {
		switch strings.ToLower(v) {
		case "true", "1", "on", "yes":
			return true
		}
	}
	return false
}
