package b2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"backuprepo/internal/apperr"
)

// fakeDeleter records each DeleteObjects batch and can fail a given call,
// either as a request error or with per-key Errors in a "successful" response.
type fakeDeleter struct {
	batches  []int // keys per call
	failCall int   // 1-based call to fail; 0 = never
	keyErrs  bool  // fail via out.Errors instead of a request error
}

func (f *fakeDeleter) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.batches = append(f.batches, len(in.Delete.Objects))
	if !aws.ToBool(in.Delete.Quiet) {
		return nil, errors.New("test expects Quiet mode")
	}
	if len(f.batches) != f.failCall {
		return &s3.DeleteObjectsOutput{}, nil
	}
	if !f.keyErrs {
		return nil, errors.New("connection reset")
	}
	return &s3.DeleteObjectsOutput{Errors: []types.Error{
		{Key: aws.String("k"), VersionId: aws.String("v7"), Code: aws.String("AccessDenied"), Message: aws.String("denied")},
		{Key: aws.String("k"), VersionId: aws.String("v8"), Code: aws.String("AccessDenied"), Message: aws.String("denied")},
	}}, nil
}

func versionIDs(n int) []types.ObjectIdentifier {
	ids := make([]types.ObjectIdentifier, n)
	for i := range ids {
		ids[i] = types.ObjectIdentifier{Key: aws.String("k"), VersionId: aws.String(fmt.Sprintf("v%d", i))}
	}
	return ids
}

// TestDeleteObjectBatchesSplitsAt1000 is the regression for sending every
// version in one DeleteObjects request: S3 rejects more than 1000 keys.
func TestDeleteObjectBatchesSplitsAt1000(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []int
	}{
		{1, []int{1}},
		{1000, []int{1000}},
		{1001, []int{1000, 1}},
		{2500, []int{1000, 1000, 500}},
	} {
		f := &fakeDeleter{}
		if err := deleteObjectBatches(context.Background(), f, "bkt", "k", versionIDs(tc.n)); err != nil {
			t.Fatalf("%d ids: %v", tc.n, err)
		}
		if fmt.Sprint(f.batches) != fmt.Sprint(tc.want) {
			t.Errorf("%d ids: batches %v, want %v", tc.n, f.batches, tc.want)
		}
	}
}

// TestDeleteObjectBatchesReportsKeyErrors is the regression for ignoring the
// per-key Errors of a DeleteObjects response (Quiet mode returns only those),
// which reported a partial delete as success.
func TestDeleteObjectBatchesReportsKeyErrors(t *testing.T) {
	f := &fakeDeleter{failCall: 1, keyErrs: true}
	err := deleteObjectBatches(context.Background(), f, "bkt", "k", versionIDs(5))
	if !errors.Is(err, apperr.ErrDeleteFailed) {
		t.Fatalf("err = %v, want ErrDeleteFailed", err)
	}
	for _, s := range []string{"2 of 5 not deleted", "v7", "AccessDenied"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q missing %q", err, s)
		}
	}
}

// TestDeleteObjectBatchesStopsAtFailedBatch checks a failing batch (request
// error or key errors) is reported and later batches are not sent.
func TestDeleteObjectBatchesStopsAtFailedBatch(t *testing.T) {
	for _, keyErrs := range []bool{false, true} {
		f := &fakeDeleter{failCall: 2, keyErrs: keyErrs}
		err := deleteObjectBatches(context.Background(), f, "bkt", "k", versionIDs(2500))
		if !errors.Is(err, apperr.ErrDeleteFailed) {
			t.Fatalf("keyErrs=%v: err = %v, want ErrDeleteFailed", keyErrs, err)
		}
		if fmt.Sprint(f.batches) != "[1000 1000]" {
			t.Errorf("keyErrs=%v: batches %v, want [1000 1000] (stop after the failure)", keyErrs, f.batches)
		}
	}
}

// TestDeleteObjectBatchesEmpty sends nothing for no ids.
func TestDeleteObjectBatchesEmpty(t *testing.T) {
	f := &fakeDeleter{}
	if err := deleteObjectBatches(context.Background(), f, "bkt", "k", nil); err != nil || len(f.batches) != 0 {
		t.Fatalf("err %v, batches %v; want nil, none", err, f.batches)
	}
}
