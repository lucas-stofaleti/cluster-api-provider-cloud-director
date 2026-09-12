package controllers

import (
	"testing"

	"github.com/vmware/go-vcloud-director/v2/types/v56"
)

func entry(key, value string) *types.MetadataEntry {
	return &types.MetadataEntry{
		Key:        key,
		TypedValue: &types.MetadataTypedValue{Value: value},
	}
}

// metadataEntriesMatch guards a write to the vApp, which takes the vApp's single write
// lock. Returning true when the values differ would silently drop a needed update, so
// every case that is not provably a no-op must return false.
func TestMetadataEntriesMatch(t *testing.T) {
	const infraID = "urn:vcloud:entity:vmware:capvcdCluster:dd94e753-3241-4195-8be1-2e965d51f475"

	tests := []struct {
		name    string
		entries []*types.MetadataEntry
		want    map[string]string
		expect  bool
	}{
		{
			name:    "exact match skips the write",
			entries: []*types.MetadataEntry{entry("CapvcdInfraId", infraID)},
			want:    map[string]string{"CapvcdInfraId": infraID},
			expect:  true,
		},
		{
			name:    "match alongside unrelated keys",
			entries: []*types.MetadataEntry{entry("other", "x"), entry("CapvcdInfraId", infraID)},
			want:    map[string]string{"CapvcdInfraId": infraID},
			expect:  true,
		},
		{
			name:    "different value must write",
			entries: []*types.MetadataEntry{entry("CapvcdInfraId", "urn:vcloud:entity:vmware:capvcdCluster:other")},
			want:    map[string]string{"CapvcdInfraId": infraID},
			expect:  false,
		},
		{
			name:    "key absent must write",
			entries: []*types.MetadataEntry{entry("unrelated", "x")},
			want:    map[string]string{"CapvcdInfraId": infraID},
			expect:  false,
		},
		{
			name:    "no metadata at all must write",
			entries: nil,
			want:    map[string]string{"CapvcdInfraId": infraID},
			expect:  false,
		},
		{
			name:    "empty want is never a no-op",
			entries: []*types.MetadataEntry{entry("CapvcdInfraId", infraID)},
			want:    map[string]string{},
			expect:  false,
		},
		{
			name:    "nil want is never a no-op",
			entries: []*types.MetadataEntry{entry("CapvcdInfraId", infraID)},
			want:    nil,
			expect:  false,
		},
		{
			name:    "empty stored value does not match a real one",
			entries: []*types.MetadataEntry{entry("CapvcdInfraId", "")},
			want:    map[string]string{"CapvcdInfraId": infraID},
			expect:  false,
		},
		{
			name:    "nil entry in the slice is skipped, not treated as a match",
			entries: []*types.MetadataEntry{nil, entry("CapvcdInfraId", infraID)},
			want:    map[string]string{"CapvcdInfraId": infraID},
			expect:  true,
		},
		{
			name:    "entry with nil TypedValue must not satisfy the key",
			entries: []*types.MetadataEntry{{Key: "CapvcdInfraId"}},
			want:    map[string]string{"CapvcdInfraId": infraID},
			expect:  false,
		},
		{
			name:    "all keys must match, not just one",
			entries: []*types.MetadataEntry{entry("a", "1"), entry("b", "wrong")},
			want:    map[string]string{"a": "1", "b": "2"},
			expect:  false,
		},
		{
			name:    "multiple keys all matching",
			entries: []*types.MetadataEntry{entry("a", "1"), entry("b", "2"), entry("c", "3")},
			want:    map[string]string{"a": "1", "b": "2"},
			expect:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := metadataEntriesMatch(tt.entries, tt.want); got != tt.expect {
				t.Errorf("metadataEntriesMatch() = %v, want %v", got, tt.expect)
			}
		})
	}
}

// A nil vApp must never report a match: the caller would then skip a write it still owes.
func TestVAppMetadataMatchesRejectsNilVApp(t *testing.T) {
	want := map[string]string{"CapvcdInfraId": "urn:vcloud:entity:vmware:capvcdCluster:abc"}
	if vAppMetadataMatches(nil, want) {
		t.Error("vAppMetadataMatches(nil, want) = true, want false")
	}
}
