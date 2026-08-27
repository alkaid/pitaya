// Copyright (c) TFG Co. All Rights Reserved.
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/groups"
	"github.com/alkaid/pitaya/v2/session/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func createGroupTestApp() Pitaya {
	config := config.NewDefaultPitayaConfig()
	app := newAppTestApp(true, "testtype", Cluster, map[string]string{}, *config)
	return app
}

var isolatedGroupSequence atomic.Uint64

type invalidPagedGroupService struct {
	groups.GroupService
	page              groups.MemberPage
	removeMemberCalls int
}

type cyclicPagedGroupService struct {
	groups.GroupService
	calls int
}

type scriptedPagedGroupService struct {
	groups.GroupService
	pages map[string]groups.MemberPage
}

type countingLegacyGroupService struct {
	groups.GroupService
	members []string
	calls   atomic.Int32
}

func (s *countingLegacyGroupService) GroupMembers(ctx context.Context, _ string) ([]string, error) {
	s.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]string(nil), s.members...), nil
}

func (s *cyclicPagedGroupService) GroupMembersPage(
	_ context.Context,
	_ string,
	options groups.MemberPageOptions,
) (groups.MemberPage, error) {
	s.calls++
	if s.calls > 3 {
		return groups.MemberPage{}, errors.New("cursor cycle was not rejected")
	}
	switch options.Cursor {
	case "":
		return groups.MemberPage{Members: []string{"a"}, NextCursor: "cursor-a"}, nil
	case "cursor-a":
		return groups.MemberPage{Members: []string{"b"}, NextCursor: "cursor-b"}, nil
	default:
		return groups.MemberPage{Members: []string{"c"}, NextCursor: "cursor-a"}, nil
	}
}

func (s *scriptedPagedGroupService) GroupMembersPage(
	_ context.Context,
	_ string,
	options groups.MemberPageOptions,
) (groups.MemberPage, error) {
	page, ok := s.pages[options.Cursor]
	if !ok {
		return groups.MemberPage{}, fmt.Errorf("unexpected cursor %q", options.Cursor)
	}
	return page, nil
}

func (s *invalidPagedGroupService) GroupMembersPage(
	context.Context,
	string,
	groups.MemberPageOptions,
) (groups.MemberPage, error) {
	return s.page, nil
}

func (s *invalidPagedGroupService) GroupRemoveMember(context.Context, string, string) error {
	s.removeMemberCalls++
	return nil
}

func isolatedGroupName(t *testing.T, app Pitaya, prefix string) string {
	t.Helper()
	groupName := fmt.Sprintf("%s-%d", prefix, isolatedGroupSequence.Add(1))
	t.Cleanup(func() {
		err := app.GroupDelete(context.Background(), groupName)
		if err != nil && !errors.Is(err, constants.ErrGroupNotFound) {
			t.Errorf("delete test group %q: %v", groupName, err)
		}
	})
	return groupName
}

func createIsolatedTestGroup(t *testing.T, app Pitaya, prefix string) string {
	t.Helper()
	groupName := isolatedGroupName(t, app, prefix)
	require.NoError(t, app.GroupCreate(context.Background(), groupName))
	return groupName
}

func TestCreateDuplicatedGroup(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testCreateDuplicatedGroup")
	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)
	err = app.GroupCreate(ctx, groupName)
	assert.Error(t, err)
	assert.Equal(t, constants.ErrGroupAlreadyExists, err)
}

func TestCreateGroup(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testCreateGroup")
	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)
	count, err := app.GroupCountMembers(ctx, groupName)
	assert.NoError(t, err)
	assert.Equal(t, 0, count)
	err = app.GroupRenewTTL(ctx, groupName)
	assert.Error(t, err)
}

func TestCreateGroupWithTTL(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testCreateGroupWithTTL")
	err := app.GroupCreateWithTTL(ctx, groupName, 10)
	assert.NoError(t, err)
	count, err := app.GroupCountMembers(ctx, groupName)
	assert.NoError(t, err)
	assert.Equal(t, 0, count)
	err = app.GroupRenewTTL(ctx, groupName)
	assert.NoError(t, err)
}

func TestGroupAddMember(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	tables := []struct {
		name     string
		frontend bool
		UID      string
		err      error
	}{
		{"frontend_uid", true, "someuid1", nil},
		{"frontend_nouid", true, "", constants.ErrEmptyUID},
		{"backend_nouid", false, "", constants.ErrEmptyUID},
		{"backend_uid", false, "ola1", nil},
	}

	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testGroupAddMember")
	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s := mocks.NewMockSession(ctrl)
			s.EXPECT().GetIsFrontend().Return(table.frontend).AnyTimes()
			s.EXPECT().UID().Return(table.UID).AnyTimes()
			err = app.GroupAddMember(ctx, groupName, s.UID())
			assert.Equal(t, table.err, err)
			if err == nil {
				_, err := app.GroupContainsMember(ctx, groupName, table.UID)
				if table.err == nil {
					assert.NoError(t, err)
				} else {
					assert.Error(t, err)
				}
			}
		})
	}
}

func TestGroupAddDuplicatedMember(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testGroupAddDuplicatedMember")
	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)
	err = app.GroupAddMember(ctx, groupName, "duplicatedUid")
	assert.NoError(t, err)
	err = app.GroupAddMember(ctx, groupName, "duplicatedUid")
	assert.Error(t, err)
	assert.Equal(t, constants.ErrMemberAlreadyExists, err)
}

func TestGroupContainsMember(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	tables := []struct {
		name     string
		frontend bool
		UID      string
		err      error
	}{
		{"frontend_uid", true, "someuid2", nil},
		{"backend_uid", false, "ola2", nil},
		{"backend_nouid", false, "", constants.ErrEmptyUID},
	}

	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testGroupContainsMember")
	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s := mocks.NewMockSession(ctrl)
			s.EXPECT().GetIsFrontend().Return(table.frontend).AnyTimes()
			s.EXPECT().UID().Return(table.UID).AnyTimes()
			err = app.GroupAddMember(ctx, groupName, s.UID())
			if table.err == nil {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
			b, err := app.GroupContainsMember(ctx, groupName, table.UID)
			if table.err == nil {
				assert.True(t, b)
				assert.NoError(t, err)
			} else {
				assert.False(t, b)
				assert.Error(t, err)
			}
		})
	}
}

func TestRemove(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	tables := []struct {
		name     string
		frontend bool
		UID      string
		err      error
	}{
		{"frontend_uid", true, "someuid2", nil},
		{"backend_uid", false, "ola2", nil},
	}

	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testRemove")
	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s := mocks.NewMockSession(ctrl)
			s.EXPECT().GetIsFrontend().Return(table.frontend).AnyTimes()
			s.EXPECT().UID().Return(table.UID).AnyTimes()
			err = app.GroupAddMember(ctx, groupName, s.UID())
			assert.NoError(t, err)
			err = app.GroupRemoveMember(ctx, groupName, s.UID())
			assert.NoError(t, err)
			res, err := app.GroupContainsMember(ctx, groupName, table.UID)
			assert.NoError(t, err)
			assert.False(t, res)
		})
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	tables := []struct {
		name     string
		frontend bool
		UID      string
		err      error
	}{
		{"frontend_uid", true, "leaveSomeuid2", nil},
		{"backend_uid", false, "leaveOla2", nil},
	}

	app := createGroupTestApp()
	survivorGroup := isolatedGroupName(t, app, "testDeleteSuffix")
	deletedGroup := isolatedGroupName(t, app, "testDelete")
	err := app.GroupCreate(ctx, survivorGroup)
	assert.NoError(t, err)

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			err = app.GroupCreate(ctx, deletedGroup)
			assert.NoError(t, err)
			s := mocks.NewMockSession(ctrl)
			s.EXPECT().GetIsFrontend().Return(table.frontend).AnyTimes()
			s.EXPECT().UID().Return(table.UID).AnyTimes()
			err = app.GroupAddMember(ctx, survivorGroup, s.UID())
			assert.NoError(t, err)
			err = app.GroupAddMember(ctx, deletedGroup, s.UID())
			assert.NoError(t, err)
			err = app.GroupDelete(ctx, deletedGroup)
			assert.NoError(t, err)

			res, err := app.GroupContainsMember(ctx, survivorGroup, table.UID)
			assert.NoError(t, err)
			assert.True(t, res)
			_, err = app.GroupContainsMember(ctx, deletedGroup, table.UID)
			assert.Error(t, err)
			assert.Equal(t, constants.ErrGroupNotFound, err)
		})
	}
}

func TestRemoveAll(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	tables := []struct {
		name     string
		frontend bool
		UID      string
		err      error
	}{
		{"frontend_uid", true, "removeSomeuid2", nil},
		{"backend_uid", false, "removeOla2", nil},
	}

	app := createGroupTestApp()
	survivorGroup := isolatedGroupName(t, app, "testRemoveAllSuffix")
	clearedGroup := isolatedGroupName(t, app, "testRemoveAll")
	err := app.GroupCreate(ctx, survivorGroup)
	assert.NoError(t, err)
	err = app.GroupCreate(ctx, clearedGroup)
	assert.NoError(t, err)

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s := mocks.NewMockSession(ctrl)
			s.EXPECT().GetIsFrontend().Return(table.frontend).AnyTimes()
			s.EXPECT().UID().Return(table.UID).AnyTimes()
			err = app.GroupAddMember(ctx, survivorGroup, s.UID())
			assert.NoError(t, err)
			err = app.GroupAddMember(ctx, clearedGroup, s.UID())
			assert.NoError(t, err)
			err = app.GroupRemoveAll(ctx, clearedGroup)
			assert.NoError(t, err)

			res, err := app.GroupContainsMember(ctx, survivorGroup, table.UID)
			assert.NoError(t, err)
			assert.True(t, res)
			res, err = app.GroupContainsMember(ctx, clearedGroup, table.UID)
			assert.NoError(t, err)
			assert.False(t, res)
		})
	}
}

func TestCount(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	tables := []struct {
		name     string
		frontend bool
		UID      string
		err      error
	}{
		{"frontend_uid", true, "someuid2", nil},
		{"backend_uid", false, "ola2", nil},
	}

	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testCount")
	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)

	for _, table := range tables {
		t.Run(table.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			s := mocks.NewMockSession(ctrl)
			s.EXPECT().GetIsFrontend().Return(table.frontend).AnyTimes()
			s.EXPECT().UID().Return(table.UID).AnyTimes()
			err = app.GroupAddMember(ctx, groupName, s.UID())
			assert.NoError(t, err)
			res, err := app.GroupCountMembers(ctx, groupName)
			assert.NoError(t, err)
			assert.Equal(t, 1, res)

			err = app.GroupRemoveAll(ctx, groupName)
			assert.NoError(t, err)
		})
	}
}

func TestMembers(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	app := createGroupTestApp()
	groupName := isolatedGroupName(t, app, "testGroupMembers")
	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)
	s1 := mocks.NewMockSession(ctrl)
	s1.EXPECT().GetIsFrontend().Return(true).AnyTimes()
	s1.EXPECT().UID().Return("someid1").AnyTimes()

	s2 := mocks.NewMockSession(ctrl)
	s2.EXPECT().GetIsFrontend().Return(true).AnyTimes()
	s2.EXPECT().UID().Return("someid2").AnyTimes()

	err = app.GroupAddMember(ctx, groupName, s1.UID())
	assert.NoError(t, err)
	err = app.GroupAddMember(ctx, groupName, s2.UID())
	assert.NoError(t, err)

	res, err := app.GroupMembers(ctx, groupName)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"someid1", "someid2"}, res)
}

func TestBroadcast(t *testing.T) {
	ctx := context.Background()
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	id1 := int64(1)
	id2 := int64(2)
	uid1 := "uid1"
	uid2 := "uid2"

	s1 := mocks.NewMockSession(ctrl)
	s1.EXPECT().GetIsFrontend().Return(true).AnyTimes()
	s1.EXPECT().UID().Return("someid1").AnyTimes()
	s1.EXPECT().ID().Return(id1).AnyTimes()

	s2 := mocks.NewMockSession(ctrl)
	s2.EXPECT().GetIsFrontend().Return(true).AnyTimes()
	s2.EXPECT().ID().Return(id2).AnyTimes()

	mockSessionPool := mocks.NewMockSessionPool(ctrl)
	mockSessionPool.EXPECT().GetSessionByUID(uid1).Return(s1).Times(1)
	mockSessionPool.EXPECT().GetSessionByUID(uid2).Return(s2).Times(1)

	config := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(true, "testtype", Cluster, map[string]string{}, *config)
	replaceBuilderSessionPoolForTest(builder, mockSessionPool)
	app := builder.Build()
	groupName := isolatedGroupName(t, app, "testBroadcast")

	err := app.GroupCreate(ctx, groupName)
	assert.NoError(t, err)

	err = app.GroupAddMember(ctx, groupName, uid1)
	assert.NoError(t, err)
	err = app.GroupAddMember(ctx, groupName, uid2)
	assert.NoError(t, err)
	route := "some.route.bla"
	data := []byte("hellow")
	s1.EXPECT().Push(route, data).Times(1)
	s2.EXPECT().Push(route, data).Times(1)
	err = app.GroupBroadcast(ctx, "testtype", groupName, route, data)
	assert.NoError(t, err)
}

func TestGroupMembersPage(t *testing.T) {
	ctx := context.Background()
	app := createGroupTestApp()
	groupName := createIsolatedTestGroup(t, app, "testAppGroupMembersPage")
	for _, uid := range []string{"c", "a", "b"} {
		assert.NoError(t, app.GroupAddMember(ctx, groupName, uid))
	}

	first, err := app.GroupMembersPage(ctx, groupName, groups.MemberPageOptions{Limit: 2})
	assert.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, first.Members)
	assert.NotEmpty(t, first.NextCursor)
	second, err := app.GroupMembersPage(ctx, groupName, groups.MemberPageOptions{Limit: 2, Cursor: first.NextCursor})
	assert.NoError(t, err)
	assert.Equal(t, []string{"c"}, second.Members)
	assert.Empty(t, second.NextCursor)
}

func TestGroupMembersPageRejectsInvalidPagedBackendResponse(t *testing.T) {
	tests := []struct {
		name    string
		options groups.MemberPageOptions
		page    groups.MemberPage
		wantErr error
	}{
		{
			name:    "empty member",
			options: groups.MemberPageOptions{Limit: 2},
			page:    groups.MemberPage{Members: []string{"", "b"}, NextCursor: "next"},
			wantErr: constants.ErrEmptyUID,
		},
		{
			name:    "page exceeds requested limit",
			options: groups.MemberPageOptions{Limit: 1},
			page:    groups.MemberPage{Members: []string{"a", "b"}},
			wantErr: constants.ErrGroupPageLimit,
		},
		{
			name:    "cursor makes no progress",
			options: groups.MemberPageOptions{Limit: 1, Cursor: "opaque"},
			page:    groups.MemberPage{Members: []string{"b"}, NextCursor: "opaque"},
			wantErr: constants.ErrGroupPageCursor,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &invalidPagedGroupService{page: tt.page}
			app := &App{groups: backend}

			page, err := app.GroupMembersPage(context.Background(), "group", tt.options)

			assert.ErrorIs(t, err, tt.wantErr)
			assert.Empty(t, page)
		})
	}
}

func TestGroupMembersRejectsMultiPageCursorCycle(t *testing.T) {
	backend := &cyclicPagedGroupService{}
	app := &App{groups: backend}

	members, err := app.GroupMembers(context.Background(), "group")

	assert.ErrorIs(t, err, constants.ErrGroupPageCursor)
	assert.Nil(t, members)
	assert.Equal(t, 3, backend.calls)
}

func TestGroupMembersRejectsCrossPageOrderViolation(t *testing.T) {
	tests := []struct {
		name  string
		first []string
		next  []string
	}{
		{
			name:  "reordered pages",
			first: []string{"z"},
			next:  []string{"a"},
		},
		{
			name:  "overlapping pages",
			first: []string{"a", "b"},
			next:  []string{"b", "c"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &scriptedPagedGroupService{pages: map[string]groups.MemberPage{
				"":       {Members: test.first, NextCursor: "page-2"},
				"page-2": {Members: test.next},
			}}
			app := &App{groups: backend}

			members, err := app.GroupMembers(context.Background(), "group")

			assert.ErrorIs(t, err, constants.ErrGroupPageCursor)
			assert.Nil(t, members)
		})
	}
}

func TestGroupMembersLegacyBackendReadsSnapshotOnce(t *testing.T) {
	members := make([]string, groups.DefaultMemberPageSize+1)
	for index := range members {
		members[index] = fmt.Sprintf("member-%04d", len(members)-index)
	}
	backend := &countingLegacyGroupService{members: members}
	app := &App{groups: backend}

	got, err := app.GroupMembers(context.Background(), "group")

	require.NoError(t, err)
	require.Len(t, got, len(members))
	assert.Equal(t, "member-0001", got[0])
	assert.Equal(t, int32(1), backend.calls.Load())
}

func TestGroupMembersPageLegacyBackendHonorsCanceledContextBeforeSnapshot(t *testing.T) {
	backend := &countingLegacyGroupService{members: []string{"member"}}
	app := &App{groups: backend}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	page, err := app.GroupMembersPage(ctx, "group", groups.MemberPageOptions{Limit: 1})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, page)
	assert.Zero(t, backend.calls.Load())
}

func TestGroupBroadcastLegacyBackendReusesSnapshotAcrossPages(t *testing.T) {
	ctrl := gomock.NewController(t)
	members := []string{"legacy-c", "legacy-a", "legacy-b"}
	backend := &countingLegacyGroupService{members: members}
	pool := mocks.NewMockSessionPool(ctrl)
	for _, uid := range []string{"legacy-a", "legacy-b", "legacy-c"} {
		sess := mocks.NewMockSession(ctrl)
		sess.EXPECT().Push("onEvent", []byte("payload")).Return(nil)
		pool.EXPECT().GetSessionByUID(uid).Return(sess)
	}
	conf := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(true, "testtype", Cluster, map[string]string{}, *conf)
	replaceBuilderSessionPoolForTest(builder, pool)
	app := builder.Build().(*App)
	app.groups = backend

	result, err := app.GroupBroadcastWithOptions(
		context.Background(),
		"testtype",
		"group",
		"onEvent",
		[]byte("payload"),
		GroupBroadcastOptions{PageSize: 1, Shards: 1},
	)

	require.NoError(t, err)
	assert.Equal(t, 3, result.Succeeded)
	assert.Equal(t, int32(1), backend.calls.Load())
}

func TestGroupBroadcastRejectsCrossPageDuplicateBeforeSendingPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	backend := &scriptedPagedGroupService{pages: map[string]groups.MemberPage{
		"": {
			Members:    []string{"broadcast-a", "broadcast-b"},
			NextCursor: "page-2",
		},
		"page-2": {
			Members: []string{"broadcast-b", "broadcast-c"},
		},
	}}
	pool := mocks.NewMockSessionPool(ctrl)
	for _, uid := range []string{"broadcast-a", "broadcast-b"} {
		sess := mocks.NewMockSession(ctrl)
		sess.EXPECT().Push("onEvent", []byte("payload")).Return(nil).Times(1)
		pool.EXPECT().GetSessionByUID(uid).Return(sess).Times(1)
	}
	conf := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(true, "testtype", Cluster, map[string]string{}, *conf)
	replaceBuilderSessionPoolForTest(builder, pool)
	app := builder.Build().(*App)
	app.groups = backend

	result, err := app.GroupBroadcastWithOptions(
		context.Background(),
		"testtype",
		"group",
		"onEvent",
		[]byte("payload"),
		GroupBroadcastOptions{PageSize: 2, Shards: 1},
	)

	assert.ErrorIs(t, err, constants.ErrGroupPageCursor)
	assert.Equal(t, GroupBroadcastResult{
		Attempted:  2,
		Succeeded:  2,
		NextCursor: "page-2",
	}, result)
}

func TestGroupRemoveMemberRejectsEmptyUIDBeforeBackend(t *testing.T) {
	backend := &invalidPagedGroupService{}
	app := &App{groups: backend}

	err := app.GroupRemoveMember(context.Background(), "group", "")

	assert.ErrorIs(t, err, constants.ErrEmptyUID)
	assert.Zero(t, backend.removeMemberCalls)
}

func TestGroupBroadcastWithOptionsStablePartialResult(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	failedUID := "uid-a"
	successUID := "uid-b"
	pushErr := errors.New("push failed")
	failedSession := mocks.NewMockSession(ctrl)
	failedSession.EXPECT().Push("onEvent", []byte("payload")).Return(pushErr)
	failedSession.EXPECT().ID().Return(int64(1)).AnyTimes()
	failedSession.EXPECT().UID().Return(failedUID).AnyTimes()
	successSession := mocks.NewMockSession(ctrl)
	successSession.EXPECT().Push("onEvent", []byte("payload")).Return(nil)

	pool := mocks.NewMockSessionPool(ctrl)
	pool.EXPECT().GetSessionByUID(failedUID).Return(failedSession)
	pool.EXPECT().GetSessionByUID(successUID).Return(successSession)

	conf := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(true, "testtype", Cluster, map[string]string{}, *conf)
	replaceBuilderSessionPoolForTest(builder, pool)
	app := builder.Build()
	groupName := createIsolatedTestGroup(t, app, "testBroadcastPartial")
	assert.NoError(t, app.GroupAddMember(ctx, groupName, successUID))
	assert.NoError(t, app.GroupAddMember(ctx, groupName, failedUID))

	result, err := app.GroupBroadcastWithOptions(
		ctx,
		"testtype",
		groupName,
		"onEvent",
		[]byte("payload"),
		GroupBroadcastOptions{PageSize: 1, Shards: 2},
	)
	assert.ErrorIs(t, err, constants.ErrPushingToUsers)
	assert.Equal(t, 2, result.Attempted)
	assert.Equal(t, 1, result.Succeeded)
	assert.Equal(t, []string{failedUID}, result.FailedUIDs)
	assert.Empty(t, result.NextCursor)
}

func TestGroupBroadcastWithOptionsValidation(t *testing.T) {
	app := createGroupTestApp()
	_, err := app.GroupBroadcastWithOptions(
		context.Background(),
		"frontend",
		"group",
		"route",
		nil,
		GroupBroadcastOptions{Shards: MaxGroupBroadcastShards + 1},
	)
	assert.ErrorIs(t, err, constants.ErrGroupBroadcastOptions)
}

func TestGroupBroadcastRateLimitSpansMembershipPages(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)

	pool := mocks.NewMockSessionPool(ctrl)
	for _, uid := range []string{"rate-a", "rate-b", "rate-c"} {
		sess := mocks.NewMockSession(ctrl)
		sess.EXPECT().Push("onEvent", []byte("payload")).Return(nil)
		pool.EXPECT().GetSessionByUID(uid).Return(sess)
	}

	conf := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(true, "testtype", Cluster, map[string]string{}, *conf)
	replaceBuilderSessionPoolForTest(builder, pool)
	app := builder.Build()
	groupName := createIsolatedTestGroup(t, app, "testBroadcastRateLimitPages")
	for _, uid := range []string{"rate-a", "rate-b", "rate-c"} {
		assert.NoError(t, app.GroupAddMember(ctx, groupName, uid))
	}

	started := time.Now()
	result, err := app.GroupBroadcastWithOptions(
		ctx,
		"testtype",
		groupName,
		"onEvent",
		[]byte("payload"),
		GroupBroadcastOptions{PageSize: 1, Shards: 1, RatePerSecond: 100},
	)
	require.NoError(t, err)
	assert.Equal(t, 3, result.Succeeded)
	assert.GreaterOrEqual(t, time.Since(started), 15*time.Millisecond)
}

func TestGroupBroadcastResumesAfterLastCompletedPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	ctx, cancel := context.WithCancel(context.Background())

	firstSession := mocks.NewMockSession(ctrl)
	firstSession.EXPECT().Push("onEvent", []byte("payload")).DoAndReturn(func(string, []byte) error {
		cancel()
		return nil
	})
	secondSession := mocks.NewMockSession(ctrl)
	secondSession.EXPECT().Push("onEvent", []byte("payload")).Return(nil)
	thirdSession := mocks.NewMockSession(ctrl)
	thirdSession.EXPECT().Push("onEvent", []byte("payload")).Return(nil)

	pool := mocks.NewMockSessionPool(ctrl)
	pool.EXPECT().GetSessionByUID("resume-a").Return(firstSession)
	pool.EXPECT().GetSessionByUID("resume-b").Return(secondSession)
	pool.EXPECT().GetSessionByUID("resume-c").Return(thirdSession)

	conf := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(true, "testtype", Cluster, map[string]string{}, *conf)
	replaceBuilderSessionPoolForTest(builder, pool)
	app := builder.Build()
	groupName := createIsolatedTestGroup(t, app, "testBroadcastResumePages")
	for _, uid := range []string{"resume-a", "resume-b", "resume-c"} {
		require.NoError(t, app.GroupAddMember(context.Background(), groupName, uid))
	}

	first, err := app.GroupBroadcastWithOptions(
		ctx,
		"testtype",
		groupName,
		"onEvent",
		[]byte("payload"),
		GroupBroadcastOptions{PageSize: 1, Shards: 1},
	)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, first.Succeeded)
	require.NotEmpty(t, first.NextCursor)

	resumed, err := app.GroupBroadcastWithOptions(
		context.Background(),
		"testtype",
		groupName,
		"onEvent",
		[]byte("payload"),
		GroupBroadcastOptions{PageSize: 1, Shards: 1, Cursor: first.NextCursor},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, resumed.Succeeded)
	assert.Empty(t, resumed.NextCursor)
}

func TestGroupBroadcastCancellationStopsRemainingPushesWithoutRateLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	ctx, cancel := context.WithCancel(context.Background())

	firstSession := mocks.NewMockSession(ctrl)
	firstSession.EXPECT().Push("onEvent", []byte("payload")).DoAndReturn(func(string, []byte) error {
		cancel()
		return nil
	})
	pool := mocks.NewMockSessionPool(ctrl)
	pool.EXPECT().GetSessionByUID("cancel-a").Return(firstSession)

	conf := config.NewDefaultPitayaConfig()
	builder := newAppTestBuilder(true, "testtype", Cluster, map[string]string{}, *conf)
	replaceBuilderSessionPoolForTest(builder, pool)
	app := builder.Build()
	groupName := createIsolatedTestGroup(t, app, "testBroadcastCancelWithoutRateLimit")
	for _, uid := range []string{"cancel-a", "cancel-b", "cancel-c"} {
		require.NoError(t, app.GroupAddMember(context.Background(), groupName, uid))
	}

	result, err := app.GroupBroadcastWithOptions(
		ctx,
		"testtype",
		groupName,
		"onEvent",
		[]byte("payload"),
		GroupBroadcastOptions{PageSize: 3, Shards: 1},
	)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, result.Attempted)
	assert.Equal(t, 1, result.Succeeded)
	assert.Equal(t, []string{"cancel-b", "cancel-c"}, result.FailedUIDs)
	assert.Empty(t, result.NextCursor)
}
