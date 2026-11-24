package ownership

import (
	"reflect"
	"testing"
)

func TestParseExclusions(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  []string
		valid bool
	}{
		{name: "empty", valid: true},
		{name: "paths", value: "/tmp,/var/cache/app", want: []string{"/tmp", "/var/cache/app"}, valid: true},
		{name: "duplicate", value: "/tmp,/tmp", want: []string{"/tmp"}, valid: true},
		{name: "relative", value: "tmp"},
		{name: "root", value: "/"},
		{name: "parent", value: "/tmp/../etc"},
		{name: "dot", value: "/tmp/./cache"},
		{name: "empty item", value: "/tmp,,/var/cache"},
		{name: "trailing slash", value: "/tmp/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseExclusions(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("ParseExclusions(%q) error = %v", test.value, err)
			}
			if test.valid && !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ParseExclusions(%q) = %#v, want %#v", test.value, got, test.want)
			}
		})
	}
}
