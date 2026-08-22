package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseSourceIDsAcceptsConfiguredMaximum(t *testing.T) {
	values := make([]string, maximumVirtualSources)
	for index := range values {
		values[index] = fmt.Sprintf("virtual-%02d", index+1)
	}
	sources, err := parseSourceIDs(strings.Join(values, ","))
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != maximumVirtualSources {
		t.Fatalf("sources=%d want=%d", len(sources), maximumVirtualSources)
	}
}

func TestParseSourceIDsRejectsAboveConfiguredMaximum(t *testing.T) {
	values := make([]string, maximumVirtualSources+1)
	for index := range values {
		values[index] = fmt.Sprintf("virtual-%02d", index+1)
	}
	if _, err := parseSourceIDs(strings.Join(values, ",")); err == nil {
		t.Fatal("expected source count validation error")
	}
}
