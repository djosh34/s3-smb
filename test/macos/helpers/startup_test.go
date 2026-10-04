// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import "testing"

func TestStartup(t *testing.T) {
	for phase, prompt := range map[string]string{"initialize": "Initialize a genuinely empty S3 dataset?\nContinue? [yes/no]: ", "recover": "Recover metadata from meta/dump-2099\nContinue? [yes/no]: ", "restart": ""} {
		t.Run(phase, func(t *testing.T) {
			startup := NewStartup(phase)
			text := prompt + `{"msg":"SMB serving"}`
			answers, readiness := 0, 0
			for _, value := range []byte(text) {
				answer, ready, point, err := startup.Observe([]byte{value})
				must(t, err)
				if answer {
					answers++
				}
				if ready {
					readiness++
					if phase == "recover" && point != "meta/dump-2099" {
						t.Fatal(point)
					}
				}
			}
			expected := 1
			if phase == "restart" {
				expected = 0
			}
			if answers != expected || readiness != 1 {
				t.Fatal(answers, readiness)
			}
			answer, ready, _, err := startup.Observe([]byte(text))
			must(t, err)
			if answer || ready {
				t.Fatal("repeated consent or readiness")
			}
		})
	}
}

func TestStartupRefusesWrongConsent(t *testing.T) {
	for _, test := range []struct{ phase, text string }{
		{"initialize", `{"msg":"SMB serving"}`},
		{"recover", `{"msg":"SMB serving"}`},
		{"restart", "Initialize a genuinely empty S3 dataset?\nContinue? [yes/no]: "},
		{"initialize", "Recover metadata from meta/dump-2099\nContinue? [yes/no]: "},
		{"recover", "Initialize a genuinely empty S3 dataset?\nContinue? [yes/no]: "},
		{"initialize", "Something else\nContinue? [yes/no]: "},
	} {
		answer, ready, _, err := NewStartup(test.phase).Observe([]byte(test.text))
		if err == nil || answer || ready {
			t.Fatal(test, "accepted unexpected consent")
		}
	}
}

func TestSelectBackup(t *testing.T) {
	path := "/Volumes/.timemachine/id/2099.backup/2099.backup"
	for _, identifier := range []string{"", "2099.backup"} {
		selected, err := SelectBackup([]string{path}, path, identifier)
		must(t, err)
		if selected != path {
			t.Fatal(selected)
		}
	}
	for _, test := range []struct {
		latest, identifier string
		paths              []string
	}{
		{path, "", nil},
		{"/source", "", []string{path}},
		{"", "missing.backup", []string{path}},
		{path, "", []string{path, path}},
		{"", "2099.backup", []string{path, path}},
		{"relative", "", []string{"relative"}},
		{"/Volumes/2099.inProgress", "", []string{"/Volumes/2099.inProgress"}},
	} {
		if _, err := SelectBackup(test.paths, test.latest, test.identifier); err == nil {
			t.Fatal(test, "accepted incomplete backup")
		}
	}
}
