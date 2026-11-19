package controller

import "testing"

func TestKeycloakEgressPort(t *testing.T) {
	cases := []struct {
		url  string
		want int32
	}{
		{"http://172.18.0.6:30081", 30081}, // kind: the Keycloak NodePort
		{"http://keycloak.keycloak.svc", 80},
		{"https://auth.example.com", 443},
		{"https://auth.example.com:8443/", 8443},
		{"", 80},
		{"http://host:notaport", 80},
	}
	for _, c := range cases {
		if got := keycloakEgressPort(&platformConfig{KeycloakURL: c.url}); got != c.want {
			t.Errorf("keycloakEgressPort(%q) = %d, want %d", c.url, got, c.want)
		}
	}
	if got := keycloakEgressPort(nil); got != 80 {
		t.Errorf("keycloakEgressPort(nil) = %d, want 80", got)
	}
}
