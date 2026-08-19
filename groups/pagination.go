package groups

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/alkaid/pitaya/v2/constants"
)

const (
	DefaultMemberPageSize = 256
	MaxMemberPageSize     = 4096
	memberSortChunkSize   = 2048
)

// MemberPageOptions selects one bounded, lexicographically ordered member page.
// Cursor is opaque and must be passed back unchanged.
type MemberPageOptions struct {
	Limit  int
	Cursor string
}

// MemberPage is a stable page of group members. NextCursor is empty on the
// final page.
type MemberPage struct {
	Members    []string
	NextCursor string
}

// BroadcastOptions bounds the work admitted by a group broadcast. Shards are
// deterministic UID partitions; each shard is processed serially while shards
// run concurrently. RatePerSecond limits aggregate send starts when positive.
type BroadcastOptions struct {
	PageSize      int
	Shards        int
	RatePerSecond int
	Cursor        string
}

// BroadcastResult is returned even when a subset fails. FailedUIDs is always
// sorted, making retries and comparisons deterministic.
type BroadcastResult struct {
	Attempted  int
	Succeeded  int
	FailedUIDs []string
	NextCursor string
}

// PagedGroupService is an optional capability. Keeping it separate from
// GroupService preserves compatibility with custom group backends.
type PagedGroupService interface {
	GroupMembersPage(ctx context.Context, groupName string, options MemberPageOptions) (MemberPage, error)
}

// MemberSnapshot owns one immutable, ordered copy of a legacy backend's
// membership. Higher-level operations can reuse it across pages without
// repeatedly fetching and sorting the full group.
type MemberSnapshot struct {
	members []string
}

// NewMemberSnapshot copies, validates, and orders members. Sorting checks ctx
// between bounded chunks and throughout merge passes so cancellation does not
// wait for an unbounded full-set sort.
func NewMemberSnapshot(ctx context.Context, members []string) (*MemberSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ordered := make([]string, len(members))
	for start := 0; start < len(members); start += memberSortChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+memberSortChunkSize, len(members))
		copy(ordered[start:end], members[start:end])
	}
	if err := validateMemberUIDs(ordered); err != nil {
		return nil, err
	}
	if err := sortMemberUIDs(ctx, ordered); err != nil {
		return nil, err
	}
	if err := validateStrictMemberOrder(ordered); err != nil {
		return nil, err
	}
	return &MemberSnapshot{members: ordered}, nil
}

// Members returns an ordered copy of the complete snapshot.
func (s *MemberSnapshot) Members(ctx context.Context) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	members := make([]string, len(s.members))
	for start := 0; start < len(s.members); start += memberSortChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+memberSortChunkSize, len(s.members))
		copy(members[start:end], s.members[start:end])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return members, nil
}

// Page returns one bounded page from the immutable snapshot.
func (s *MemberSnapshot) Page(ctx context.Context, options MemberPageOptions) (MemberPage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return MemberPage{}, err
	}
	options, err := normalizeMemberPageOptions(options)
	if err != nil {
		return MemberPage{}, err
	}
	cursor, err := decodeMemberCursor(options.Cursor)
	if err != nil {
		return MemberPage{}, err
	}
	start := 0
	if cursor != "" {
		start = sort.Search(len(s.members), func(i int) bool { return s.members[i] > cursor })
	}
	end := min(start+options.Limit, len(s.members))
	page := MemberPage{Members: append([]string(nil), s.members[start:end]...)}
	if err := ctx.Err(); err != nil {
		return MemberPage{}, err
	}
	if end < len(s.members) && len(page.Members) > 0 {
		page.NextCursor = encodeMemberCursor(page.Members[len(page.Members)-1])
	}
	if err := validateBuiltInMemberPage(options, page); err != nil {
		return MemberPage{}, err
	}
	return page, nil
}

func normalizeMemberPageOptions(options MemberPageOptions) (MemberPageOptions, error) {
	if options.Limit == 0 {
		options.Limit = DefaultMemberPageSize
	}
	if options.Limit < 1 || options.Limit > MaxMemberPageSize {
		return MemberPageOptions{}, fmt.Errorf("%w: got %d, want 1..%d", constants.ErrGroupPageLimit, options.Limit, MaxMemberPageSize)
	}
	return options, nil
}

func encodeMemberCursor(uid string) string {
	if uid == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(uid))
}

func decodeMemberCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	uid, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(uid) == 0 {
		return "", fmt.Errorf("%w: %q", constants.ErrGroupPageCursor, cursor)
	}
	return string(uid), nil
}

func validateMemberUIDs(members []string) error {
	for index, uid := range members {
		if uid == "" {
			return fmt.Errorf("group page member %d: %w", index, constants.ErrEmptyUID)
		}
	}
	return nil
}

func validateStrictMemberOrder(members []string) error {
	for index := 1; index < len(members); index++ {
		if members[index-1] >= members[index] {
			return fmt.Errorf(
				"%w: members must be strictly ordered at indexes %d and %d",
				constants.ErrGroupPageCursor,
				index-1,
				index,
			)
		}
	}
	return nil
}

// ValidateMemberPage checks the backend-independent page contract without
// interpreting the opaque cursor format used by a custom PagedGroupService.
func ValidateMemberPage(options MemberPageOptions, page MemberPage) error {
	options, err := normalizeMemberPageOptions(options)
	if err != nil {
		return err
	}
	if len(page.Members) > options.Limit {
		return fmt.Errorf(
			"%w: backend returned %d members for limit %d",
			constants.ErrGroupPageLimit,
			len(page.Members),
			options.Limit,
		)
	}
	if err := validateMemberUIDs(page.Members); err != nil {
		return err
	}
	if err := validateStrictMemberOrder(page.Members); err != nil {
		return err
	}
	if page.NextCursor != "" && len(page.Members) == 0 {
		return fmt.Errorf("%w: non-final page has no members", constants.ErrGroupPageCursor)
	}
	if page.NextCursor != "" && page.NextCursor == options.Cursor {
		return fmt.Errorf("%w: backend cursor did not advance", constants.ErrGroupPageCursor)
	}
	return nil
}

func validateBuiltInMemberPage(options MemberPageOptions, page MemberPage) error {
	if err := ValidateMemberPage(options, page); err != nil {
		return err
	}
	if page.NextCursor == "" {
		return nil
	}
	nextMember, err := decodeMemberCursor(page.NextCursor)
	if err != nil {
		return err
	}
	if nextMember != page.Members[len(page.Members)-1] {
		return fmt.Errorf("%w: cursor does not match the last member", constants.ErrGroupPageCursor)
	}
	return nil
}

// PaginateMembers provides a deterministic fallback for custom GroupService
// implementations that have not implemented PagedGroupService yet.
func PaginateMembers(members []string, options MemberPageOptions) (MemberPage, error) {
	snapshot, err := NewMemberSnapshot(context.Background(), members)
	if err != nil {
		return MemberPage{}, err
	}
	return snapshot.Page(context.Background(), options)
}

// CursorAfterMember returns an opaque cursor positioned immediately after uid.
// It is useful for resumable higher-level operations that stop inside a page.
func CursorAfterMember(uid string) string {
	return encodeMemberCursor(uid)
}

func sortMemberUIDs(ctx context.Context, members []string) error {
	for start := 0; start < len(members); start += memberSortChunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+memberSortChunkSize, len(members))
		sort.Strings(members[start:end])
	}
	if len(members) <= memberSortChunkSize {
		return ctx.Err()
	}

	scratch := make([]string, len(members))
	source, destination := members, scratch
	sourceIsMembers := true
	for width := memberSortChunkSize; width < len(members); width *= 2 {
		for start := 0; start < len(members); start += 2 * width {
			if err := ctx.Err(); err != nil {
				return err
			}
			middle := min(start+width, len(members))
			end := min(start+2*width, len(members))
			if err := mergeMemberUIDRuns(ctx, destination[start:end], source[start:middle], source[middle:end]); err != nil {
				return err
			}
		}
		source, destination = destination, source
		sourceIsMembers = !sourceIsMembers
		if width > len(members)/2 {
			break
		}
	}
	if !sourceIsMembers {
		copy(members, source)
	}
	return ctx.Err()
}

func mergeMemberUIDRuns(ctx context.Context, destination, left, right []string) error {
	leftIndex, rightIndex := 0, 0
	for index := range destination {
		if index%memberSortChunkSize == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if rightIndex >= len(right) || (leftIndex < len(left) && left[leftIndex] <= right[rightIndex]) {
			destination[index] = left[leftIndex]
			leftIndex++
			continue
		}
		destination[index] = right[rightIndex]
		rightIndex++
	}
	return nil
}
