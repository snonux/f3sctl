package httpapi

import (
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// maxBodyBytes caps how much of a request body is read. Every action here
// takes at most one small field, so anything larger is a mistake or an attack.
const maxBodyBytes = 64 << 10

// parseCGIRequest builds a request from the CGI environment and stdin.
//
// The variables used here were verified against bozohttpd 20260508 on NetBSD
// 11.0: PATH_INFO carries the sub-path below SCRIPT_NAME, arbitrary request
// headers arrive as HTTP_<UPPER_SNAKE>, and CONTENT_LENGTH bounds the body on
// stdin. See the f3s-raspberry-pi skill, references/bootstrap-netbsd-pi.md.
func parseCGIRequest(stdin io.Reader) (request, error) {
	req := request{
		Method: strings.ToUpper(orDefault(os.Getenv("REQUEST_METHOD"), "GET")),
		Path:   normalisePath(os.Getenv("PATH_INFO")),
		// bozohttpd exports X-API-Key as HTTP_X_API_KEY.
		APIKey: os.Getenv("HTTP_X_API_KEY"),
		Form:   url.Values{},
	}

	q, err := url.ParseQuery(os.Getenv("QUERY_STRING"))
	if err != nil {
		// A malformed query string is not worth rejecting the request over;
		// the fields it would have carried simply are not there.
		q = url.Values{}
	}
	req.Query = q

	if req.Method == "POST" {
		form, err := readForm(stdin)
		if err != nil {
			return req, err
		}
		req.Form = form
	}
	return req, nil
}

// readForm reads and parses an application/x-www-form-urlencoded body.
//
// A body is optional: most actions take no fields at all, and a client sending
// none is normal rather than an error.
func readForm(stdin io.Reader) (url.Values, error) {
	length, err := strconv.Atoi(os.Getenv("CONTENT_LENGTH"))
	if err != nil || length <= 0 {
		return url.Values{}, nil
	}
	if length > maxBodyBytes {
		length = maxBodyBytes
	}

	body, err := io.ReadAll(io.LimitReader(stdin, int64(length)))
	if err != nil {
		return nil, err
	}

	form, err := url.ParseQuery(string(body))
	if err != nil {
		return url.Values{}, nil
	}
	return form, nil
}

// normalisePath maps an empty or missing PATH_INFO to the root, and strips a
// trailing slash so "/status" and "/status/" are the same resource.
func normalisePath(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return strings.TrimSuffix(p, "/")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
