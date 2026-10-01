package main

import "testing"

func TestVerificationSummaryRejectsNUL(t *testing.T) {
	if _, err := unmarshalAnnotation(`{"summary":"text\u0000text","category":"work","tags":[]}`); err == nil {
		t.Fatal("accepted a summary that PostgreSQL and epistula-api reject")
	}
}

func TestVerificationModelRejectsNUL(t *testing.T) {
	c := DefaultConfig()
	c.MailAPI.Token = "fixture-token"
	c.deriveAnnotationModel()
	if err := c.Validate(); err != nil {
		t.Fatalf("baseline config invalid: %v", err)
	}
	c.Worker.AnnotationModel = "model\x00suffix"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted a model identity PostgreSQL and epistula-api reject")
	}
}
