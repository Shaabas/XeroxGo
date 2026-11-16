package printer

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseIPPGetJobs_SingleJob(t *testing.T) {
	output := []byte(`"/usr/share/cups/ipptool/get-jobs.test":
    Get-Jobs:
        attributes-charset (charset) = utf-8
        attributes-natural-language (naturalLanguage) = en
        printer-uri (uri) = ipp://localhost:631/printers/BrotherQueue
        requested-attributes (1setOf keyword) = job-id,job-uri,job-state,job-state-reasons,job-name,job-originating-user-name,job-media-sheets,job-media-sheets-completed,job-impressions,job-impressions-completed
    Get pending jobs                                                     [PASS]
        RECEIVED: 260 bytes in response
        status-code = successful-ok (successful-ok)
        attributes-charset (charset) = utf-8
        attributes-natural-language (naturalLanguage) = en
        job-uri (uri) = ipp://localhost:631/jobs/101
        job-id (integer) = 101
        job-name (nameWithoutLanguage) = job-test-101
        job-originating-user-name (nameWithoutLanguage) = operator
        job-state (enum) = processing
        job-state-reasons (keyword) = job-printing
        job-impressions-completed (integer) = 1
        job-media-sheets-completed (integer) = 1
`)

	jobs, ok := parseIPPGetJobs("BrotherQueue", output)
	if !ok {
		t.Fatalf("expected parseIPPGetJobs to succeed")
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.ID != "BrotherQueue-101" {
		t.Errorf("expected ID BrotherQueue-101, got %s", j.ID)
	}
	if j.JobNumber != 101 {
		t.Errorf("expected JobNumber 101, got %d", j.JobNumber)
	}
	if j.Name != "job-test-101" {
		t.Errorf("expected Name job-test-101, got %s", j.Name)
	}
	if j.User != "operator" {
		t.Errorf("expected User operator, got %s", j.User)
	}
	if j.State != CUPSJobStateProcessing {
		t.Errorf("expected State processing, got %s", j.State)
	}
	if j.SheetsCompleted != 1 {
		t.Errorf("expected SheetsCompleted 1, got %d", j.SheetsCompleted)
	}
	if len(j.Reasons) != 1 || j.Reasons[0] != "job-printing" {
		t.Errorf("expected Reasons [job-printing], got %v", j.Reasons)
	}
}

func TestParseIPPGetJobs_MultipleJobs(t *testing.T) {
	output := []byte(`"/usr/share/cups/ipptool/get-jobs.test":
    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
        attributes-charset (charset) = utf-8
        attributes-natural-language (naturalLanguage) = en
        job-uri (uri) = ipp://localhost:631/jobs/101
        job-id (integer) = 101
        job-name (nameWithoutLanguage) = job-aaa
        job-originating-user-name (nameWithoutLanguage) = user1
        job-state (enum) = processing
        job-media-sheets-completed (integer) = 2
        -- separator --
        job-uri (uri) = ipp://localhost:631/jobs/102
        job-id (integer) = 102
        job-name (nameWithoutLanguage) = job-bbb
        job-originating-user-name (nameWithoutLanguage) = user2
        job-state (enum) = pending-held
        job-state-reasons (keyword) = job-hold-until-specified
        job-media-sheets-completed (integer) = 0
`)

	jobs, ok := parseIPPGetJobs("TestQueue", output)
	if !ok {
		t.Fatalf("expected parseIPPGetJobs to succeed")
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}

	if jobs[0].ID != "TestQueue-101" || jobs[0].Name != "job-aaa" || jobs[0].State != CUPSJobStateProcessing || jobs[0].SheetsCompleted != 2 {
		t.Errorf("job 0 mismatch: %+v", jobs[0])
	}
	if jobs[1].ID != "TestQueue-102" || jobs[1].Name != "job-bbb" || jobs[1].State != CUPSJobStateHeld || jobs[1].SheetsCompleted != 0 {
		t.Errorf("job 1 mismatch: %+v", jobs[1])
	}
}

func TestParseIPPGetJobs_ZeroJobs(t *testing.T) {
	output := []byte(`"/usr/share/cups/ipptool/get-jobs.test":
    Get pending jobs                                                     [PASS]
        RECEIVED: 80 bytes in response
        status-code = successful-ok (successful-ok)
        attributes-charset (charset) = utf-8
        attributes-natural-language (naturalLanguage) = en
`)

	jobs, ok := parseIPPGetJobs("TestQueue", output)
	if !ok {
		t.Fatalf("expected parseIPPGetJobs to succeed")
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestParseIPPGetJobs_ErrorStatus(t *testing.T) {
	output := []byte(`"/usr/share/cups/ipptool/get-jobs.test":
    Get pending jobs                                                     [FAIL]
        status-code = client-error-not-found (client-error-not-found)
`)

	jobs, ok := parseIPPGetJobs("TestQueue", output)
	if ok || jobs != nil {
		t.Fatalf("expected parseIPPGetJobs to fail on non-ok status")
	}
}

func TestParseLPQJobTitle(t *testing.T) {
	lpqOutput := `BrotherQueue is ready
operator: 1st                             [job 101 localhost]
        my-custom-title-101                     1024 bytes
operator: 2nd                             [job 102 localhost]
        second-title-102                        2048 bytes
`

	title101 := parseLPQJobTitle(lpqOutput, "101")
	if title101 != "my-custom-title-101" {
		t.Errorf("expected my-custom-title-101, got %q", title101)
	}

	title102 := parseLPQJobTitle(lpqOutput, "102")
	if title102 != "second-title-102" {
		t.Errorf("expected second-title-102, got %q", title102)
	}

	title999 := parseLPQJobTitle(lpqOutput, "999")
	if title999 != "" {
		t.Errorf("expected empty string for non-existent job, got %q", title999)
	}
}

func TestEnumerateCUPSQueueJobs_FallbackLPStat(t *testing.T) {
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			// Simulate missing ipptool test file or ipptool failure
			return nil, errors.New("ipptool: command not found")
		}
		if name == "lpstat" {
			for i, arg := range args {
				if arg == "-W" && i+1 < len(args) && args[i+1] == "not-completed" {
					return []byte("BrotherQ-201 operator 1024 Mon Nov 16 12:00:00 2026\nBrotherQ-202 operator 2048 Mon Nov 16 12:01:00 2026\n"), nil
				}
				if arg == "-p" {
					return []byte("printer BrotherQ is now printing BrotherQ-201. enabled since ...\n"), nil
				}
			}
		}
		if name == "lpq" {
			return []byte(`BrotherQ is ready
operator: 1st                             [job 201 localhost]
        job-fallback-title-201                  1024 bytes
`), nil
		}
		return []byte(""), nil
	}

	jobs, err := EnumerateActiveCUPSJobs(context.Background(), runner, "BrotherQ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs from lpstat fallback, got %d", len(jobs))
	}
	if jobs[0].ID != "BrotherQ-201" || jobs[0].JobNumber != 201 || jobs[0].User != "operator" {
		t.Errorf("job 0 mismatch: %+v", jobs[0])
	}
	if jobs[0].Name != "job-fallback-title-201" {
		t.Errorf("expected fallback title job-fallback-title-201, got %q", jobs[0].Name)
	}
}

func TestFindUnlinkedJobCandidates_SingleMatch(t *testing.T) {
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/501
        job-id (integer) = 501
        job-name (nameWithoutLanguage) = job-unlinked-target
        job-originating-user-name (nameWithoutLanguage) = testuser
        job-state (enum) = processing
        job-media-sheets-completed (integer) = 0
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	match, err := FindUnlinkedJobCandidates(context.Background(), runner, "TestQueue", "job-unlinked-target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match.MatchingState != "single_match" {
		t.Errorf("expected matching state single_match, got %s", match.MatchingState)
	}
	if match.CandidateCount != 1 {
		t.Errorf("expected 1 candidate, got %d", match.CandidateCount)
	}
	if len(match.Candidates) != 1 || match.Candidates[0].ID != "TestQueue-501" {
		t.Errorf("unexpected candidate details: %+v", match.Candidates)
	}
	if !strings.Contains(match.DiagnosticMsg, "TestQueue-501") ||
		!strings.Contains(match.DiagnosticMsg, "ownership unverified") ||
		!strings.Contains(match.DiagnosticMsg, "physical output uncertain") {
		t.Errorf("diagnostic message lacks required safety caveats: %s", match.DiagnosticMsg)
	}
}

func TestFindUnlinkedJobCandidates_MultipleMatches_TitleCollision(t *testing.T) {
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/601
        job-id (integer) = 601
        job-name (nameWithoutLanguage) = job-collision
        job-originating-user-name (nameWithoutLanguage) = userA
        job-state (enum) = processing
        -- separator --
        job-uri (uri) = ipp://localhost:631/jobs/602
        job-id (integer) = 602
        job-name (nameWithoutLanguage) = job-collision
        job-originating-user-name (nameWithoutLanguage) = userB
        job-state (enum) = pending-held
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	match, err := FindUnlinkedJobCandidates(context.Background(), runner, "TestQueue", "job-collision")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match.MatchingState != "multiple_matches" {
		t.Errorf("expected matching state multiple_matches, got %s", match.MatchingState)
	}
	if match.CandidateCount != 2 {
		t.Errorf("expected 2 candidates, got %d", match.CandidateCount)
	}
	if !strings.Contains(match.DiagnosticMsg, "title collision/duplicate detected") ||
		!strings.Contains(match.DiagnosticMsg, "TestQueue-601") ||
		!strings.Contains(match.DiagnosticMsg, "TestQueue-602") {
		t.Errorf("diagnostic message lacks collision details: %s", match.DiagnosticMsg)
	}
}

func TestFindUnlinkedJobCandidates_ZeroMatches(t *testing.T) {
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					// Active queue has an unrelated job
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/701
        job-id (integer) = 701
        job-name (nameWithoutLanguage) = unrelated-job
        job-originating-user-name (nameWithoutLanguage) = userA
        job-state (enum) = processing
`), nil
				}
				if strings.Contains(arg, "get-completed-jobs.test") {
					// Completed history has no matching jobs
					return []byte(`    Get completed jobs                                                   [PASS]
        status-code = successful-ok (successful-ok)
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	match, err := FindUnlinkedJobCandidates(context.Background(), runner, "TestQueue", "my-target-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match.MatchingState != "zero_matches" {
		t.Errorf("expected matching state zero_matches, got %s", match.MatchingState)
	}
	if match.CandidateCount != 0 {
		t.Errorf("expected 0 candidates, got %d", match.CandidateCount)
	}
	if !strings.Contains(match.DiagnosticMsg, "0 candidate CUPS jobs") {
		t.Errorf("expected zero candidates in diagnostic message, got %s", match.DiagnosticMsg)
	}
}

func TestFindUnlinkedJobCandidates_StaleJobInCompletedHistory(t *testing.T) {
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					// 0 active jobs
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
`), nil
				}
				if strings.Contains(arg, "get-completed-jobs.test") {
					// Completed history has a job matching the title
					return []byte(`    Get completed jobs                                                   [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/801
        job-id (integer) = 801
        job-name (nameWithoutLanguage) = stale-target-job
        job-originating-user-name (nameWithoutLanguage) = olduser
        job-state (enum) = completed
        job-media-sheets-completed (integer) = 1
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	match, err := FindUnlinkedJobCandidates(context.Background(), runner, "TestQueue", "stale-target-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match.MatchingState != "history_match" {
		t.Errorf("expected matching state history_match, got %s", match.MatchingState)
	}
	if match.CandidateCount != 1 {
		t.Errorf("expected 1 candidate in history, got %d", match.CandidateCount)
	}
	if !strings.Contains(match.DiagnosticMsg, "detected 1 candidate completed CUPS job(s) in history") ||
		!strings.Contains(match.DiagnosticMsg, "ownership unverified without durable CUPS ID") {
		t.Errorf("diagnostic message lacks required caveats: %s", match.DiagnosticMsg)
	}
}

func TestFindUnlinkedJobCandidates_EnumerationError_FailsClosed(t *testing.T) {
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("cupsd connection refused")
	}

	match, err := FindUnlinkedJobCandidates(context.Background(), runner, "TestQueue", "target-job")
	if err == nil {
		t.Fatalf("expected error from failed CUPS runner")
	}
	if match.MatchingState != "enumeration_error" {
		t.Errorf("expected matching state enumeration_error, got %s", match.MatchingState)
	}
	if !strings.Contains(match.DiagnosticMsg, "error querying CUPS queue jobs") {
		t.Errorf("unexpected diagnostic message: %s", match.DiagnosticMsg)
	}
}

func TestFindUnlinkedJobCandidates_UnrelatedJobsUntouched(t *testing.T) {
	var modifyingCalls atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "cancel" || name == "lp" {
			modifyingCalls.Add(1)
		}
		if name == "ipptool" {
			for _, arg := range args {
				if strings.Contains(arg, "get-jobs.test") {
					return []byte(`    Get pending jobs                                                     [PASS]
        status-code = successful-ok (successful-ok)
        job-uri (uri) = ipp://localhost:631/jobs/901
        job-id (integer) = 901
        job-name (nameWithoutLanguage) = job-unrelated-customer
        job-originating-user-name (nameWithoutLanguage) = customerA
        job-state (enum) = processing
`), nil
				}
				if strings.Contains(arg, "get-completed-jobs.test") {
					return []byte(`    Get completed jobs                                                   [PASS]
        status-code = successful-ok (successful-ok)
`), nil
				}
			}
		}
		return []byte(""), nil
	}

	match, err := FindUnlinkedJobCandidates(context.Background(), runner, "TestQueue", "my-target-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match.MatchingState != "zero_matches" {
		t.Errorf("expected zero_matches, got %s", match.MatchingState)
	}
	if modifyingCalls.Load() > 0 {
		t.Fatalf("safety violation: FindUnlinkedJobCandidates issued %d modifying calls", modifyingCalls.Load())
	}
}
