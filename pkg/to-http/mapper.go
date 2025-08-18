package tohttp

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/nats-io/nats.go"
)

/*
	subject := strings.Join([]string{
		sm.SubjectPrefix,
		sm.Request.URL.Scheme,
		sm.Request.Method,
		sm.Host,

sm.Path //rules on how its split, _ = :, . = / ?,
if domain reverse com.bar.foo to foo.bar.com

	},
subject=h8s.http.POST.zitadel.authorize
".")
*/

// func isPv6 then add brackets around the host
func FromNatsMessageToHttpRequest(natsMsg nats.Msg) (*http.Request, error) {
	// Assuming natsMsg is a byte slice containing the HTTP request data
	// Convert it to a string and parse it as an HTTP request
	parts := strings.Split(natsMsg.Subject, ".")
	requestURLScheme := parts[1]
	httpMethod := parts[2]
	host := parts[3]
	path := httpPathFromParts(parts)
	url := httpURLFromParts(requestURLScheme, host, path)

	body := natsMsg.Data
	req, err := http.NewRequest(httpMethod, url, bytes.NewReader(body))
	// TODO: this
	_ = req
	if err != nil {
		return nil, fmt.Errorf("failed to read HTTP request: %w", err)
	}
	return nil, nil
}

func httpURLFromParts(requestURLScheme, host, path string) string {
	return requestURLScheme + "://" + host + "/" + path
}

func httpPathFromParts(parts []string, isDomain bool) string {
	iMin := 3
	if !isDomain {
	}
	path := ""
	for i := len(parts) - 1; i > iMin; i-- {
		path += parts[i]
		if i-1 > iMin {
			path += "/"
		}
	}
	return path
}
