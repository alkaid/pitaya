// Copyright (c) nano Author and TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package pitaya

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/groups"
	"github.com/alkaid/pitaya/v2/logger"
)

const (
	DefaultGroupBroadcastPageSize = groups.DefaultMemberPageSize
	DefaultGroupBroadcastShards   = 8
	MaxGroupBroadcastShards       = 64
)

type GroupBroadcastOptions = groups.BroadcastOptions
type GroupBroadcastResult = groups.BroadcastResult

// Group represents an agglomeration of UIDs which is used to manage
// users. Data sent to the group will be sent to all users in it.

// GroupCreate creates a group
func (app *App) GroupCreate(ctx context.Context, groupName string) error {
	return app.groups.GroupCreate(ctx, groupName)
}

// GroupCreateWithTTL creates a group with given TTL
func (app *App) GroupCreateWithTTL(ctx context.Context, groupName string, ttlTime time.Duration) error {
	return app.groups.GroupCreateWithTTL(ctx, groupName, ttlTime)
}

// GroupMembers returns all member's UIDs
func (app *App) GroupMembers(ctx context.Context, groupName string) ([]string, error) {
	if _, ok := app.groups.(groups.PagedGroupService); !ok {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		members, err := app.groups.GroupMembers(ctx, groupName)
		if err != nil {
			return nil, err
		}
		snapshot, err := groups.NewMemberSnapshot(ctx, members)
		if err != nil {
			return nil, err
		}
		return snapshot.Members(ctx)
	}

	members := make([]string, 0)
	cursor := ""
	previousLastUID := ""
	seenCursors := make(map[string]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := app.GroupMembersPage(ctx, groupName, groups.MemberPageOptions{
			Limit:  groups.DefaultMemberPageSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		previousLastUID, err = validateGroupPageBoundary(previousLastUID, page.Members)
		if err != nil {
			return nil, err
		}
		members = append(members, page.Members...)
		if page.NextCursor == "" {
			return members, nil
		}
		if err := rememberGroupCursor(seenCursors, page.NextCursor); err != nil {
			return nil, err
		}
		cursor = page.NextCursor
	}
}

// GroupMembersPage returns one bounded member page. Custom group backends that
// only implement GroupService use a deterministic in-memory fallback.
func (app *App) GroupMembersPage(ctx context.Context, groupName string, options groups.MemberPageOptions) (groups.MemberPage, error) {
	if err := ctx.Err(); err != nil {
		return groups.MemberPage{}, err
	}
	if paged, ok := app.groups.(groups.PagedGroupService); ok {
		page, err := paged.GroupMembersPage(ctx, groupName, options)
		if err != nil {
			return groups.MemberPage{}, err
		}
		if err := groups.ValidateMemberPage(options, page); err != nil {
			return groups.MemberPage{}, err
		}
		return page, nil
	}
	members, err := app.groups.GroupMembers(ctx, groupName)
	if err != nil {
		return groups.MemberPage{}, err
	}
	snapshot, err := groups.NewMemberSnapshot(ctx, members)
	if err != nil {
		return groups.MemberPage{}, err
	}
	return snapshot.Page(ctx, options)
}

// GroupBroadcast pushes the message to all members inside group
func (app *App) GroupBroadcast(ctx context.Context, frontendType, groupName, route string, v interface{}) error {
	_, err := app.GroupBroadcastWithOptions(ctx, frontendType, groupName, route, v, GroupBroadcastOptions{})
	return err
}

// GroupBroadcastWithOptions pages group membership and applies deterministic,
// bounded sharding. It continues after per-user failures and returns the stable
// partial result together with an aggregate error.
func (app *App) GroupBroadcastWithOptions(
	ctx context.Context,
	frontendType, groupName, route string,
	v interface{},
	options GroupBroadcastOptions,
) (GroupBroadcastResult, error) {
	options, err := normalizeGroupBroadcastOptions(options)
	if err != nil {
		return GroupBroadcastResult{}, err
	}
	logger.Zap.Debug("Type=Broadcast", zap.String("Route", route), zap.Any("Data", v))

	result := GroupBroadcastResult{NextCursor: options.Cursor}
	var broadcastErrs []error
	cursor := options.Cursor
	previousLastUID := ""
	seenCursors := make(map[string]struct{})
	if cursor != "" {
		seenCursors[cursor] = struct{}{}
	}
	pacer := newBroadcastPacer(options.RatePerSecond)
	pageSource, err := app.groupMemberPageSource(ctx, groupName)
	if err != nil {
		return result, err
	}
	for {
		if err := ctx.Err(); err != nil {
			result.NextCursor = cursor
			return result, err
		}
		page, err := pageSource(ctx, groups.MemberPageOptions{
			Limit:  options.PageSize,
			Cursor: cursor,
		})
		if err != nil {
			result.NextCursor = cursor
			return result, err
		}
		previousLastUID, err = validateGroupPageBoundary(previousLastUID, page.Members)
		if err != nil {
			result.NextCursor = cursor
			return result, err
		}
		if err := rememberGroupCursor(seenCursors, page.NextCursor); err != nil {
			result.NextCursor = cursor
			return result, err
		}

		pageResult, pageErr := app.broadcastMemberPage(ctx, page.Members, frontendType, route, v, options, pacer)
		result.Attempted += pageResult.Attempted
		result.Succeeded += pageResult.Succeeded
		result.FailedUIDs = append(result.FailedUIDs, pageResult.FailedUIDs...)
		if pageErr != nil {
			broadcastErrs = append(broadcastErrs, pageErr)
			if ctxErr := ctx.Err(); ctxErr != nil {
				sort.Strings(result.FailedUIDs)
				result.NextCursor = cursor
				return result, errors.Join(append(broadcastErrs, ctxErr)...)
			}
		}
		cursor = page.NextCursor
		result.NextCursor = cursor
		if cursor == "" {
			sort.Strings(result.FailedUIDs)
			if len(result.FailedUIDs) > 0 {
				broadcastErrs = append(broadcastErrs, constants.ErrPushingToUsers)
			}
			return result, errors.Join(broadcastErrs...)
		}
	}
}

func validateGroupPageBoundary(previousLastUID string, members []string) (string, error) {
	if len(members) == 0 {
		return previousLastUID, nil
	}
	if previousLastUID != "" && members[0] <= previousLastUID {
		return previousLastUID, fmt.Errorf(
			"%w: page starts with %q after %q",
			constants.ErrGroupPageCursor,
			members[0],
			previousLastUID,
		)
	}
	return members[len(members)-1], nil
}

func (app *App) groupMemberPageSource(
	ctx context.Context,
	groupName string,
) (func(context.Context, groups.MemberPageOptions) (groups.MemberPage, error), error) {
	if _, ok := app.groups.(groups.PagedGroupService); ok {
		return func(pageCtx context.Context, options groups.MemberPageOptions) (groups.MemberPage, error) {
			return app.GroupMembersPage(pageCtx, groupName, options)
		}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	members, err := app.groups.GroupMembers(ctx, groupName)
	if err != nil {
		return nil, err
	}
	snapshot, err := groups.NewMemberSnapshot(ctx, members)
	if err != nil {
		return nil, err
	}
	return snapshot.Page, nil
}

func rememberGroupCursor(seen map[string]struct{}, cursor string) error {
	if cursor == "" {
		return nil
	}
	if _, exists := seen[cursor]; exists {
		return fmt.Errorf("%w: backend cursor cycle", constants.ErrGroupPageCursor)
	}
	seen[cursor] = struct{}{}
	return nil
}

func normalizeGroupBroadcastOptions(options GroupBroadcastOptions) (GroupBroadcastOptions, error) {
	if options.PageSize == 0 {
		options.PageSize = DefaultGroupBroadcastPageSize
	}
	if options.Shards == 0 {
		options.Shards = DefaultGroupBroadcastShards
	}
	if options.PageSize < 1 || options.PageSize > groups.MaxMemberPageSize {
		return GroupBroadcastOptions{}, fmt.Errorf("%w: page size must be 1..%d", constants.ErrGroupBroadcastOptions, groups.MaxMemberPageSize)
	}
	if options.Shards < 1 || options.Shards > MaxGroupBroadcastShards {
		return GroupBroadcastOptions{}, fmt.Errorf("%w: shards must be 1..%d", constants.ErrGroupBroadcastOptions, MaxGroupBroadcastShards)
	}
	if options.RatePerSecond < 0 || options.RatePerSecond > 1_000_000 {
		return GroupBroadcastOptions{}, fmt.Errorf("%w: rate must be 0..1000000", constants.ErrGroupBroadcastOptions)
	}
	return options, nil
}

type broadcastPacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newBroadcastPacer(rate int) *broadcastPacer {
	if rate <= 0 {
		return nil
	}
	return &broadcastPacer{interval: time.Second / time.Duration(rate)}
}

func (p *broadcastPacer) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	wait := time.Until(p.next)
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (app *App) broadcastMemberPage(
	ctx context.Context,
	members []string,
	frontendType, route string,
	v interface{},
	options GroupBroadcastOptions,
	pacer *broadcastPacer,
) (GroupBroadcastResult, error) {
	if len(members) == 0 {
		return GroupBroadcastResult{}, nil
	}
	shardCount := min(options.Shards, len(members))
	shards := make([][]string, shardCount)
	for _, uid := range members {
		h := fnv.New32a()
		_, _ = h.Write([]byte(uid))
		index := int(h.Sum32() % uint32(shardCount))
		shards[index] = append(shards[index], uid)
	}

	type outcome struct {
		uid       string
		attempted bool
		err       error
	}
	outcomes := make(chan outcome, len(members))
	var wg sync.WaitGroup
	for _, shard := range shards {
		shard := shard
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, uid := range shard {
				if err := pacer.wait(ctx); err != nil {
					outcomes <- outcome{uid: uid, err: err}
					continue
				}
				failed, err := app.SendPushToUsers(route, v, []string{uid}, frontendType)
				if err == nil && len(failed) > 0 {
					err = constants.ErrPushingToUsers
				}
				outcomes <- outcome{uid: uid, attempted: true, err: err}
			}
		}()
	}
	wg.Wait()
	close(outcomes)

	result := GroupBroadcastResult{}
	var errs []error
	for outcome := range outcomes {
		if outcome.attempted {
			result.Attempted++
		}
		if outcome.err != nil {
			result.FailedUIDs = append(result.FailedUIDs, outcome.uid)
			errs = append(errs, outcome.err)
			continue
		}
		result.Succeeded++
	}
	sort.Strings(result.FailedUIDs)
	return result, errors.Join(errs...)
}

func (app *App) sendDataToMembers(uids []string, frontendType, route string, v interface{}) error {
	errUids, err := app.SendPushToUsers(route, v, uids, frontendType)
	if err != nil {
		logger.Zap.Error("Group push message error", zap.Strings("uids", errUids), zap.Error(err))
		return err
	}
	return nil
}

// GroupContainsMember checks whether an UID is contained in group or not
func (app *App) GroupContainsMember(ctx context.Context, groupName, uid string) (bool, error) {
	if uid == "" {
		return false, constants.ErrEmptyUID
	}
	return app.groups.GroupContainsMember(ctx, groupName, uid)
}

// GroupAddMember adds UID to group
func (app *App) GroupAddMember(ctx context.Context, groupName, uid string) error {
	if uid == "" {
		return constants.ErrEmptyUID
	}
	logger.Zap.Debug("Add user to group", zap.String("group", groupName), zap.String("UID", uid))
	return app.groups.GroupAddMember(ctx, groupName, uid)
}

// GroupRemoveMember removes specified UID from group
func (app *App) GroupRemoveMember(ctx context.Context, groupName, uid string) error {
	if uid == "" {
		return constants.ErrEmptyUID
	}
	logger.Zap.Debug("Remove user from group", zap.String("group", groupName), zap.String("UID", uid))
	return app.groups.GroupRemoveMember(ctx, groupName, uid)
}

// GroupRemoveAll clears all UIDs
func (app *App) GroupRemoveAll(ctx context.Context, groupName string) error {
	return app.groups.GroupRemoveAll(ctx, groupName)
}

// GroupCountMembers get current member amount in group
func (app *App) GroupCountMembers(ctx context.Context, groupName string) (int, error) {
	return app.groups.GroupCountMembers(ctx, groupName)
}

// GroupRenewTTL renews group with the initial TTL
func (app *App) GroupRenewTTL(ctx context.Context, groupName string) error {
	return app.groups.GroupRenewTTL(ctx, groupName)
}

// GroupDelete deletes whole group, including UIDs and base group
func (app *App) GroupDelete(ctx context.Context, groupName string) error {
	return app.groups.GroupDelete(ctx, groupName)
}
