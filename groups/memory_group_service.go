package groups

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/co"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
)

var (
	memoryGroupsMu sync.RWMutex
	memoryGroups   map[string]*MemoryGroup
	memoryOnce     sync.Once
)

// MemoryGroupService base in server memory solution
type MemoryGroupService struct {
}

// MemoryGroup is the struct stored in each group key(which is the name of the group)
type MemoryGroup struct {
	Uids        []string
	LastRefresh int64
	TTL         int64
}

// NewMemoryGroupService returns a new group instance
func NewMemoryGroupService(config config.MemoryGroupConfig) *MemoryGroupService {
	memoryOnce.Do(func() {
		memoryGroups = make(map[string]*MemoryGroup)
		co.Go(func() { groupTTLCleanup(config.TickDuration) })
	})
	return &MemoryGroupService{}
}

func groupTTLCleanup(duration time.Duration) {
	for now := range time.Tick(duration) {
		memoryGroupsMu.Lock()
		for groupName, mg := range memoryGroups {
			if mg.TTL != 0 && now.UnixNano()-mg.LastRefresh > mg.TTL {
				delete(memoryGroups, groupName)
			}
		}
		memoryGroupsMu.Unlock()
	}
}

// GroupCreate creates a group without TTL
func (c *MemoryGroupService) GroupCreate(ctx context.Context, groupName string) error {
	memoryGroupsMu.Lock()
	defer memoryGroupsMu.Unlock()

	if _, ok := memoryGroups[groupName]; ok {
		return constants.ErrGroupAlreadyExists
	}

	memoryGroups[groupName] = &MemoryGroup{}
	return nil
}

// GroupCreateWithTTL creates a group with TTL, which the go routine will clean later
func (c *MemoryGroupService) GroupCreateWithTTL(ctx context.Context, groupName string, ttlTime time.Duration) error {
	memoryGroupsMu.Lock()
	defer memoryGroupsMu.Unlock()

	if _, ok := memoryGroups[groupName]; ok {
		return constants.ErrGroupAlreadyExists
	}

	memoryGroups[groupName] = &MemoryGroup{LastRefresh: time.Now().UnixNano(), TTL: ttlTime.Nanoseconds()}
	return nil
}

// GroupMembers returns all member's UID in given group
func (c *MemoryGroupService) GroupMembers(ctx context.Context, groupName string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	memoryGroupsMu.RLock()
	defer memoryGroupsMu.RUnlock()

	mg, ok := memoryGroups[groupName]
	if !ok {
		return nil, constants.ErrGroupNotFound
	}

	uids := make([]string, len(mg.Uids))
	copy(uids, mg.Uids)

	return uids, nil
}

// GroupMembersPage returns one bounded page ordered by UID.
func (c *MemoryGroupService) GroupMembersPage(ctx context.Context, groupName string, options MemberPageOptions) (MemberPage, error) {
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

	memoryGroupsMu.RLock()
	mg, ok := memoryGroups[groupName]
	if !ok {
		memoryGroupsMu.RUnlock()
		return MemberPage{}, constants.ErrGroupNotFound
	}
	start := 0
	if cursor != "" {
		start = sort.Search(len(mg.Uids), func(i int) bool { return mg.Uids[i] > cursor })
	}
	end := min(start+options.Limit, len(mg.Uids))
	page := MemberPage{Members: append([]string(nil), mg.Uids[start:end]...)}
	hasMore := end < len(mg.Uids)
	memoryGroupsMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return MemberPage{}, err
	}
	if hasMore && len(page.Members) > 0 {
		page.NextCursor = encodeMemberCursor(page.Members[len(page.Members)-1])
	}
	if err := validateBuiltInMemberPage(options, page); err != nil {
		return MemberPage{}, err
	}
	return page, nil
}

// GroupContainsMember check whether an UID is contained in given group or not
func (c *MemoryGroupService) GroupContainsMember(ctx context.Context, groupName, uid string) (bool, error) {
	if uid == "" {
		return false, constants.ErrEmptyUID
	}
	memoryGroupsMu.RLock()
	defer memoryGroupsMu.RUnlock()

	mg, ok := memoryGroups[groupName]
	if !ok {
		return false, constants.ErrGroupNotFound
	}

	_, contains := elementIndex(mg.Uids, uid)
	return contains, nil
}

// GroupAddMember adds UID to group
func (c *MemoryGroupService) GroupAddMember(ctx context.Context, groupName, uid string) error {
	if uid == "" {
		return constants.ErrEmptyUID
	}
	memoryGroupsMu.Lock()
	defer memoryGroupsMu.Unlock()

	mg, ok := memoryGroups[groupName]
	if !ok {
		return constants.ErrGroupNotFound
	}

	index, contains := elementIndex(mg.Uids, uid)
	if contains {
		return constants.ErrMemberAlreadyExists
	}

	mg.Uids = append(mg.Uids, "")
	copy(mg.Uids[index+1:], mg.Uids[index:])
	mg.Uids[index] = uid
	memoryGroups[groupName] = mg
	return nil
}

// GroupRemoveMember removes specific UID from group
func (c *MemoryGroupService) GroupRemoveMember(ctx context.Context, groupName, uid string) error {
	if uid == "" {
		return constants.ErrEmptyUID
	}
	memoryGroupsMu.Lock()
	defer memoryGroupsMu.Unlock()

	mg, ok := memoryGroups[groupName]
	if !ok {
		return constants.ErrGroupNotFound
	}
	index, contains := elementIndex(mg.Uids, uid)
	if contains {
		copy(mg.Uids[index:], mg.Uids[index+1:])
		mg.Uids[len(mg.Uids)-1] = ""
		mg.Uids = mg.Uids[:len(mg.Uids)-1]
		memoryGroups[groupName] = mg
		return nil
	}

	return constants.ErrMemberNotFound
}

// GroupRemoveAll clears all UIDs from group
func (c *MemoryGroupService) GroupRemoveAll(ctx context.Context, groupName string) error {
	memoryGroupsMu.Lock()
	defer memoryGroupsMu.Unlock()

	mg, ok := memoryGroups[groupName]
	if !ok {
		return constants.ErrGroupNotFound
	}

	mg.Uids = []string{}
	return nil
}

// GroupDelete deletes the whole group, including members and base group
func (c *MemoryGroupService) GroupDelete(ctx context.Context, groupName string) error {
	memoryGroupsMu.Lock()
	defer memoryGroupsMu.Unlock()

	_, ok := memoryGroups[groupName]
	if !ok {
		return constants.ErrGroupNotFound
	}

	delete(memoryGroups, groupName)
	return nil
}

// GroupCountMembers get current member amount in group
func (c *MemoryGroupService) GroupCountMembers(ctx context.Context, groupName string) (int, error) {
	memoryGroupsMu.RLock()
	defer memoryGroupsMu.RUnlock()

	mg, ok := memoryGroups[groupName]
	if !ok {
		return 0, constants.ErrGroupNotFound
	}

	return len(mg.Uids), nil
}

// GroupRenewTTL will renew lease TTL
func (c *MemoryGroupService) GroupRenewTTL(ctx context.Context, groupName string) error {
	memoryGroupsMu.Lock()
	defer memoryGroupsMu.Unlock()

	mg, ok := memoryGroups[groupName]
	if !ok {
		return constants.ErrGroupNotFound
	}

	if mg.TTL != 0 {
		mg.LastRefresh = time.Now().UnixNano()
		return nil
	}
	return constants.ErrMemoryTTLNotFound
}
