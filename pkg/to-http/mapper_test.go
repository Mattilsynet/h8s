package tohttp

import (
	"testing"
)

func Test_httpPathFromPaths(t *testing.T) {
	type args struct {
		parts []string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			// requestURLScheme := parts[1]
			// httpMethod := parts[2]
			// host := parts[3]
			"[h8s, http, POST, com, zitadel, lol, stuff, tada] becomes tada/stuff/lol",
			args{
				parts: []string{"h8s", "http", "POST", "com", "zitadel", "lol", "stuff", "tada"},
			},
			"tada/stuff/lol",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := httpPathFromParts(tt.args.parts); got != tt.want {
				t.Errorf("httpPathFromPaths() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_httpURLFromParts(t *testing.T) {
	type args struct {
		requestURLScheme string
		host             string
		path             string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			"[http, com, zitadel, authorize] becomes http://zitadel.com/authorize",
			args{},
			"",
		},
		{
			"[127, 0, 0, 1_8080, authorize] becomes http://127.0.0.1:8080/authorize",
			args{},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := httpURLFromParts(tt.args.requestURLScheme, tt.args.host, tt.args.path); got != tt.want {
				t.Errorf("httpURLFromParts() = %v, want %v", got, tt.want)
			}
		})
	}
}
