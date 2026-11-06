package t3

import (
	"net/url"
	"testing"
)

func TestRuntimePort(t *testing.T) {
	cases := []struct {
		name   string
		origin *url.URL
		want   int
	}{
		{"http default", &url.URL{Scheme: "http", Host: "127.0.0.1"}, 80},
		{"https default", &url.URL{Scheme: "https", Host: "localhost"}, 443},
		{"other scheme default", &url.URL{Scheme: "ws", Host: "localhost"}, 80},
		{"explicit http", &url.URL{Scheme: "http", Host: "127.0.0.1:3773"}, 3773},
		{"explicit https", &url.URL{Scheme: "https", Host: "mini.example:8443"}, 8443},
		{"lowest", &url.URL{Scheme: "http", Host: "127.0.0.1:1"}, 1},
		{"highest", &url.URL{Scheme: "http", Host: "[::1]:65535"}, 65535},
		{"zero", &url.URL{Scheme: "http", Host: "127.0.0.1:0"}, 0},
		{"above range", &url.URL{Scheme: "https", Host: "127.0.0.1:65536"}, 0},
		{"overflow", &url.URL{Scheme: "http", Host: "127.0.0.1:99999999999999999999"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runtimePort(tc.origin)
			if tc.want == 0 {
				if err == nil || got != 0 {
					t.Fatalf("runtimePort(%s) = %d, %v; want an invalid port error", tc.origin, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("runtimePort(%s) = %d, %v; want %d", tc.origin, got, err, tc.want)
			}
		})
	}
}
