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
