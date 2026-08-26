package goo

import "testing"

func TestValidBucket(t *testing.T) {
	valid := []string{"images", "my-bucket", "ab", "a", "bucket123", "a.b-c_d"}
	invalid := []string{
		"", "AB", "Bucket", "has space", "has/slash", "../etc",
		".hidden", "-lead", "trail-", strings63plus(),
		"con", "nul", "com1", "lpt9", "NUL", "COM1", // windows reserved, case-insensitive
	}

	for _, b := range valid {
		if !ValidBucket(b) {
			t.Errorf("expected bucket %q to be valid", b)
		}
	}
	for _, b := range invalid {
		if ValidBucket(b) {
			t.Errorf("expected bucket %q to be invalid", b)
		}
	}
}

func TestValidKey(t *testing.T) {
	valid := []string{
		"cat.jpg",
		"images/cat.jpg",
		"a/b/c/d.jpg",
		"model-7.onnx",
		"data_2024.parquet",
		"path/to/file-v1.2.txt",
	}
	invalid := []string{
		"",
		"/abs",
		"trailing/",
		"double//slash",
		"../escape",
		"a/../../b",
		"..",
		"./x",
		"a\x00b",
		"with space",
		"with\\backslash",
	}

	for _, k := range valid {
		if !ValidKey(k) {
			t.Errorf("expected key %q to be valid", k)
		}
	}
	for _, k := range invalid {
		if ValidKey(k) {
			t.Errorf("expected key %q to be invalid (path-traversal guard)", k)
		}
	}
}

func TestCheckRef(t *testing.T) {
	if err := CheckRef("images", "cat.jpg"); err != nil {
		t.Fatalf("valid ref rejected: %v", err)
	}
	if err := CheckRef("../etc", "passwd"); err == nil {
		t.Fatal("path-traversal bucket should be rejected")
	}
	if err := CheckRef("images", "../../etc/passwd"); err == nil {
		t.Fatal("path-traversal key should be rejected")
	}
}

func TestObjectRef(t *testing.T) {
	o := Object{Bucket: "b", Key: "k"}
	if o.Ref() != "b/k" {
		t.Fatalf("Ref() = %q", o.Ref())
	}
}

func strings63plus() string {
	s := ""
	for i := 0; i < 64; i++ {
		s += "a"
	}
	return s
}
