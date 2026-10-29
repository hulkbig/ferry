package backuppolicy

import "testing"

func TestPolicyRejectsUnsafeDestinationAndUnboundedRetention(t *testing.T) {
	for _, prefix := range []string{"other-service/data", "ferry/../other", "ferry/", "ferry/a//b", "ferry/a b", "ferry/a?x"} {
		p := Default()
		p.S3Prefix = prefix
		if Validate(p) == nil {
			t.Fatalf("accepted %q", prefix)
		}
	}
	for _, hours := range []int{0, 1, 25, 721} {
		p := Default()
		p.IntervalHours = hours
		if Validate(p) == nil {
			t.Fatalf("accepted interval %d", hours)
		}
	}
	for _, copies := range []int{0, 8} {
		p := Default()
		p.LocalCopies = copies
		if Validate(p) == nil {
			t.Fatalf("accepted retention %d", copies)
		}
	}
	if err := Validate(Default()); err != nil {
		t.Fatal(err)
	}
}

func TestS3ConnectionValidation(t *testing.T) {
	valid := Connection{Endpoint: "https://s3.example.com", Bucket: "my-backups", Region: "eu-west-1", PathStyle: true}
	if err := ValidateConnection(valid); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"file:///tmp/a", "https://key:secret@s3.example.com", "https://s3.example.com/path", "https://s3.example.com?key=secret", "https://s3.example.com#fragment", "https://"} {
		c := valid
		c.Endpoint = endpoint
		if ValidateConnection(c) == nil {
			t.Fatalf("accepted endpoint %s", endpoint)
		}
	}
	c := valid
	c.Bucket = "../other"
	if ValidateConnection(c) == nil {
		t.Fatal("invalid bucket accepted")
	}
	c = valid
	c.CredentialsRef = "oauth:account"
	if ValidateConnection(c) == nil {
		t.Fatal("foreign secret reference accepted")
	}
	c = valid
	c.Region = ""
	if ValidateConnection(c) == nil {
		t.Fatal("missing region accepted")
	}
}

func TestBothBackupNamespacesKeepPathGuards(t *testing.T) {
	for _, namespace := range []string{"ferry/", "smart-api/"} {
		for _, tail := range []string{"scheduled", "team/daily", "../other", "", "a//b", "a b", "a?x", "a/.."} {
			p := Default()
			p.S3Prefix = namespace + tail
			valid := tail == "scheduled" || tail == "team/daily"
			if (Validate(p) == nil) != valid {
				t.Fatalf("prefix %q", p.S3Prefix)
			}
		}
	}
}

func TestScheduledPolicyRejectsHistory(t *testing.T) {
	p := Default()
	p.IncludeHistory = true
	if Validate(p) == nil {
		t.Fatal("scheduled history backup allowed")
	}
}
