package taskstate

import "testing"

func TestDeliveryContractCannotBeWeakenedOnResume(t *testing.T) {
	var s State
	d := &DeliveryContract{Files: []DeliveryFile{{Path: "src/result.go", Changed: true}}, Commit: true}
	seeded, err := s.SeedDelivery(d)
	if err != nil || !seeded {
		t.Fatal(seeded, err)
	}
	s.Delivery.BaseHead = "baseline"
	s.Delivery.Files[0].BaseBlob = "blob"
	s.Delivery.Files[0].Baseline = &DeliverySnapshot{Exists: true, Kind: "regular", SHA256: "native-digest", Size: 7, Mode: 0600}
	s.DeliveryResult = &DeliveryResult{Status: DeliveryUnavailable, RunID: "r1", Issues: []DeliveryIssue{{Phase: "verification", Path: "src/result.go", Reason: "read failed"}}}
	d.Files[0].Path = "mutated"
	if s.Delivery.Files[0].Path != "src/result.go" {
		t.Fatal("caller mutated durable contract")
	}
	raw, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := Decode(3, raw)
	if err != nil {
		t.Fatal(err)
	}
	same := &DeliveryContract{Files: []DeliveryFile{{Path: "src/result.go", Changed: true}}, Commit: true}
	if seeded, err := resumed.SeedDelivery(same); err != nil || seeded {
		t.Fatal(seeded, err)
	}
	if resumed.Delivery.BaseHead != "baseline" || resumed.Delivery.Files[0].BaseBlob != "blob" {
		t.Fatal("baseline lost")
	}
	if resumed.Delivery.Files[0].Baseline.SHA256 != "native-digest" || resumed.DeliveryResult.Issues[0].Reason != "read failed" {
		t.Fatal("native evidence lost on resume")
	}
	same.Files[0].Absent = true
	if _, err := resumed.SeedDelivery(same); err == nil {
		t.Fatal("changed absence requirement accepted on resume")
	}
	same.Files[0].Absent = false
	same.Commit = false
	if _, err := resumed.SeedDelivery(same); err == nil {
		t.Fatal("weakened contract accepted")
	}
	for _, path := range []string{"../escape", "/absolute", ".git/config", ":(glob)**", "-x", "file\nspoof"} {
		if err := (DeliveryContract{Files: []DeliveryFile{{Path: path}}}).Validate(); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
}

// A branch requirement Git could never satisfy is a host error, refused without running Git.
func TestDeliveryBranchNameIsValidatedWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	bad := []string{"bad..branch", "a b", "x.lock", "a/x.lock", ".hidden", "a/.b", "a/", "/a", "a//b", "a@{1}",
		"a~1", "a^", "a:b", "a?", "a*", "a[b", "a\\b", "end.", "HEAD", "-x", "tab\tname", "del\x7f"}
	for _, b := range bad {
		if err := (DeliveryContract{Branch: b}).Validate(); err == nil {
			t.Errorf("accepted branch %q", b)
		}
	}
	for _, b := range []string{"fix/result", "feature-1", "a.b", "release/v1.2", "user@host", "a@b"} {
		if err := (DeliveryContract{Branch: b}).Validate(); err != nil {
			t.Errorf("refused branch %q: %v", b, err)
		}
	}
	var s State
	if _, err := s.SeedDelivery(&DeliveryContract{Branch: "bad..branch"}); err == nil || s.Delivery != nil {
		t.Fatalf("Start accepted an invalid branch: %v %+v", err, s.Delivery)
	}
}
