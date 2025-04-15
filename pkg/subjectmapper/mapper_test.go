package subjectmapper

import (
	"net/http"
	"net/url"
	"testing"
)

func TestIsIP(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{
			name:  "Valid IPv4",
			input: "127.0.0.1",
			want:  true,
		},
		{
			name:  "Valid IPv6",
			input: "2001:db8::1",
			want:  true,
		},
		{
			name:  "Bracketed Valid IPv6 with port",
			input: "[2001:db8::1]:8080",
			want:  true,
		},
		{
			name:  "Bracketed IPv6",
			input: "[2001:db8::1]",
			want:  true,
		},
		{
			name:  "Plain domain",
			input: "example.com",
			want:  false,
		},
		{
			name:  "Empty string",
			input: "",
			want:  false,
		},
		{
			name:  "Random string",
			input: "hello-world",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIP(tt.input)
			if got != tt.want {
				t.Errorf("isIP(%q) = %v; want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSubjectMapperProcessHost(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "Reverses simple domain",
			input: "example.com",
			want:  "com.example",
		},
		{
			name:  "Reverses multi-part domain",
			input: "foo.bar.example.com",
			want:  "com.example.bar.foo",
		},
		{
			name:  "IP with no port",
			input: "127.0.0.1",
			want:  "127.0.0.1",
		},
		{
			name:  "Parses IP with port correctly",
			input: "127.0.0.1:8080",
			want:  "127.0.0.1",
		},
		{
			name:  "Parses IPv6 with port correctly",
			input: "[2001:db8::1]:443",
			want:  "2001_db8__1",
		},
		{
			name:  "Parses IPv6 with no port and brackets",
			input: "[2001:db8::1]",
			want:  "2001_db8__1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := &SubjectMap{}
			sm.processHost(tt.input)

			if sm.Host != tt.want {
				t.Errorf("processHost(%q) = %q; want %q", tt.input, sm.Host, tt.want)
			}
		})
	}
}

func TestSubjectMapperPublishSubject(t *testing.T) {
	tests := []struct {
		name          string
		subjectPrefix string
		scheme        string
		method        string
		host          string
		path          string
		want          string
	}{
		{
			name:          "Basic HTTP GET",
			subjectPrefix: "prefix",
			scheme:        "http",
			method:        "GET",
			host:          "example.com",
			path:          "some/path",
			want:          "prefix.http.GET.example.com.some/path",
		},
		{
			name:          "HTTPS POST with no path",
			subjectPrefix: "srv",
			scheme:        "https",
			method:        "POST",
			host:          "api.myservice.local",
			path:          "",
			want:          "srv.https.POST.api.myservice.local.",
		},
		// Add more cases as needed, for example an empty SubjectPrefix,
		// custom methods, odd hostnames, or path variations
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake request with the specified scheme and method
			req := &http.Request{
				Method: tt.method,
				URL: &url.URL{
					Scheme: tt.scheme,
				},
			}

			// Instantiate the SubjectMapper with test data
			sm := &SubjectMap{
				SubjectPrefix: tt.subjectPrefix,
				Request:       req,
				Host:          tt.host,
				Path:          tt.path,
			}

			// Invoke PublishSubject() and compare
			got := sm.PublishSubject()
			if got != tt.want {
				t.Errorf("PublishSubject() = %q; want %q", got, tt.want)
			}
		})
	}
}
