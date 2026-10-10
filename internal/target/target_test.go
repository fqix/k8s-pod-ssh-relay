package target

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    Target
		wantErr bool
	}{
		{in: "default/web-0", want: Target{"default", "web-0"}},
		{in: "demo-b/box.with.dots", want: Target{"demo-b", "box.with.dots"}},
		{in: "web-0.default", want: Target{"default", "web-0"}},
		{in: "web-0.default.relay", want: Target{"default", "web-0"}},
		{in: "box.with.dots.demo-b.relay", want: Target{"demo-b", "box.with.dots"}},
		{in: "web-0", wantErr: true},          // no namespace
		{in: "web-0.relay", wantErr: true},    // ".relay" alone is not a namespace
		{in: "/web-0", wantErr: true},         // empty namespace
		{in: "default/", wantErr: true},       // empty pod
		{in: "Default/web-0", wantErr: true},  // uppercase
		{in: "my.ns/web-0", wantErr: true},    // namespaces can't contain dots
		{in: "default/web_0", wantErr: true},  // invalid character
		{in: "default/a/b", wantErr: true},    // extra slash
		{in: "default/-web", wantErr: true},   // must start alphanumeric
		{in: "web-0.default.", wantErr: true}, // trailing dot
		{in: "default/web..0", wantErr: true}, // empty label
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("Parse(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseCanonicalRejectsDottedForm(t *testing.T) {
	if _, err := ParseCanonical("web-0.default"); err == nil {
		t.Fatal("ParseCanonical accepted the dotted form")
	}
	got, err := ParseCanonical("default/web-0")
	if err != nil || got != (Target{"default", "web-0"}) {
		t.Fatalf("ParseCanonical(default/web-0) = %+v, %v", got, err)
	}
}

func TestFormsRoundTrip(t *testing.T) {
	want := Target{Namespace: "demo-b", Pod: "box.with.dots"}
	if want.String() != "demo-b/box.with.dots" || want.Dotted() != "box.with.dots.demo-b" {
		t.Fatalf("String/Dotted = %q, %q", want.String(), want.Dotted())
	}
	for _, s := range []string{want.String(), want.Dotted(), want.Dotted() + ".relay"} {
		if got, err := Parse(s); err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v", s, got, err)
		}
	}
}
