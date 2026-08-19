package groups

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/constants"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
)

var (
	clientInstance     *clientv3.Client
	transactionTimeout time.Duration
	etcdOnce           sync.Once
	etcdInitErr        error
)

// EtcdGroupService base ETCD struct solution
type EtcdGroupService struct {
}

// NewEtcdGroupService returns a new group instance
func NewEtcdGroupService(conf config.EtcdGroupServiceConfig, clientOrNil *clientv3.Client) (*EtcdGroupService, error) {
	err := initClientInstance(conf, clientOrNil)
	if err != nil {
		return nil, err
	}
	return &EtcdGroupService{}, err
}

func initClientInstance(conf config.EtcdGroupServiceConfig, clientOrNil *clientv3.Client) error {
	etcdOnce.Do(func() {
		if clientOrNil != nil {
			clientInstance = clientOrNil
		} else {
			clientInstance, etcdInitErr = createBaseClient(conf)
		}
		if etcdInitErr != nil {
			return
		}
		transactionTimeout = conf.TransactionTimeout
	})
	return etcdInitErr
}

func createBaseClient(conf config.EtcdGroupServiceConfig) (*clientv3.Client, error) {
	clientConfig, err := buildEtcdGroupClientConfig(conf)
	if err != nil {
		return nil, err
	}
	cli, err := clientv3.New(clientConfig)
	if err != nil {
		return nil, err
	}
	cli.KV = namespace.NewKV(cli.KV, conf.Prefix)
	return cli, nil
}

func buildEtcdGroupClientConfig(conf config.EtcdGroupServiceConfig) (clientv3.Config, error) {
	clientTLS, err := config.LoadClientTLS("pitaya.groups.etcd.tls", conf.TLS)
	if err != nil {
		return clientv3.Config{}, err
	}
	return clientv3.Config{
		Endpoints:   conf.Endpoints,
		DialTimeout: conf.DialTimeout,
		Username:    conf.User,
		Password:    conf.Pass,
		TLS:         clientTLS,
	}, nil
}

func groupKey(groupName string) string {
	return fmt.Sprintf("groups/%s", groupName)
}

func memberKey(groupName, uid string) string {
	return fmt.Sprintf("%s/uids/%s", groupKey(groupName), uid)
}

func getGroupKV(ctx context.Context, groupName string) (*mvccpb.KeyValue, error) {
	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	etcdRes, err := clientInstance.Get(ctxT, groupKey(groupName))
	if err != nil {
		return nil, err
	}
	if etcdRes.Count == 0 {
		return nil, constants.ErrGroupNotFound
	}
	return etcdRes.Kvs[0], nil
}

func (c *EtcdGroupService) createGroup(ctx context.Context, groupName string, leaseID clientv3.LeaseID) error {
	var etcdRes *clientv3.TxnResponse
	var err error

	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	if leaseID != 0 {
		etcdRes, err = clientInstance.Txn(ctxT).
			If(clientv3.Compare(clientv3.CreateRevision(groupKey(groupName)), "=", 0)).
			Then(clientv3.OpPut(groupKey(groupName), "", clientv3.WithLease(leaseID))).
			Commit()
	} else {
		etcdRes, err = clientInstance.Txn(ctxT).
			If(clientv3.Compare(clientv3.CreateRevision(groupKey(groupName)), "=", 0)).
			Then(clientv3.OpPut(groupKey(groupName), "")).
			Commit()
	}

	if err != nil {
		return err
	}
	if !etcdRes.Succeeded {
		return constants.ErrGroupAlreadyExists
	}
	return nil
}

// GroupCreate creates a group struct inside ETCD, without TTL
func (c *EtcdGroupService) GroupCreate(ctx context.Context, groupName string) error {
	return c.createGroup(ctx, groupName, 0)
}

// GroupCreateWithTTL creates a group struct inside ETCD, with TTL, using leaseID
func (c *EtcdGroupService) GroupCreateWithTTL(ctx context.Context, groupName string, ttlTime time.Duration) error {
	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	lease, err := clientInstance.Grant(ctxT, int64(ttlTime.Seconds()))
	if err != nil {
		return err
	}
	return c.createGroup(ctx, groupName, lease.ID)
}

// GroupMembers returns all member's UIDs
func (c *EtcdGroupService) GroupMembers(ctx context.Context, groupName string) ([]string, error) {
	members := make([]string, 0)
	cursor := ""
	for {
		page, err := c.GroupMembersPage(ctx, groupName, MemberPageOptions{
			Limit:  DefaultMemberPageSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		members = append(members, page.Members...)
		if page.NextCursor == "" {
			return members, nil
		}
		cursor = page.NextCursor
	}
}

// GroupMembersPage returns one bounded page ordered by UID.
func (c *EtcdGroupService) GroupMembersPage(ctx context.Context, groupName string, options MemberPageOptions) (MemberPage, error) {
	options, err := normalizeMemberPageOptions(options)
	if err != nil {
		return MemberPage{}, err
	}
	cursor, err := decodeMemberCursor(options.Cursor)
	if err != nil {
		return MemberPage{}, err
	}

	prefix := memberKey(groupName, "")
	start := prefix
	if cursor != "" {
		start = memberKey(groupName, cursor) + "\x00"
	}
	end := clientv3.GetPrefixRangeEnd(prefix)
	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	etcdRes, err := clientInstance.Txn(ctxT).
		If(clientv3.Compare(clientv3.CreateRevision(groupKey(groupName)), ">", 0)).
		Then(clientv3.OpGet(
			start,
			clientv3.WithRange(end),
			clientv3.WithKeysOnly(),
			clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
			clientv3.WithLimit(int64(options.Limit+1)),
		)).
		Commit()

	if err != nil {
		return MemberPage{}, err
	}
	if !etcdRes.Succeeded {
		return MemberPage{}, constants.ErrGroupNotFound
	}

	getRes := etcdRes.Responses[0].GetResponseRange()
	kvs := getRes.GetKvs()
	hasMore := len(kvs) > options.Limit
	if hasMore {
		kvs = kvs[:options.Limit]
	}
	page := MemberPage{Members: make([]string, len(kvs))}
	for i, kv := range kvs {
		page.Members[i] = string(kv.Key)[len(prefix):]
	}
	if err := validateMemberUIDs(page.Members); err != nil {
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

// GroupContainsMember checks whether a UID is contained in current group or not
func (c *EtcdGroupService) GroupContainsMember(ctx context.Context, groupName, uid string) (bool, error) {
	if uid == "" {
		return false, constants.ErrEmptyUID
	}
	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	etcdRes, err := clientInstance.Txn(ctxT).
		If(clientv3.Compare(clientv3.CreateRevision(groupKey(groupName)), ">", 0)).
		Then(clientv3.OpGet(memberKey(groupName, uid), clientv3.WithCountOnly())).
		Commit()

	if err != nil {
		return false, err
	}
	if !etcdRes.Succeeded {
		return false, constants.ErrGroupNotFound
	}
	return etcdRes.Responses[0].GetResponseRange().GetCount() > 0, nil
}

// GroupAddMember adds UID to group
func (c *EtcdGroupService) GroupAddMember(ctx context.Context, groupName, uid string) error {
	if uid == "" {
		return constants.ErrEmptyUID
	}
	var etcdRes *clientv3.TxnResponse
	kv, err := getGroupKV(ctx, groupName)
	if err != nil {
		return err
	}

	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	if kv.Lease != 0 {
		etcdRes, err = clientInstance.Txn(ctxT).
			If(clientv3.Compare(clientv3.CreateRevision(groupKey(groupName)), ">", 0),
				clientv3.Compare(clientv3.CreateRevision(memberKey(groupName, uid)), "=", 0)).
			Then(clientv3.OpPut(memberKey(groupName, uid), "", clientv3.WithLease(clientv3.LeaseID(kv.Lease)))).
			Commit()
	} else {
		etcdRes, err = clientInstance.Txn(ctxT).
			If(clientv3.Compare(clientv3.CreateRevision(groupKey(groupName)), ">", 0),
				clientv3.Compare(clientv3.CreateRevision(memberKey(groupName, uid)), "=", 0)).
			Then(clientv3.OpPut(memberKey(groupName, uid), "")).
			Commit()
	}

	if err != nil {
		return err
	}
	if !etcdRes.Succeeded {
		return constants.ErrMemberAlreadyExists
	}
	return nil
}

// GroupRemoveMember removes specified UID from group
func (c *EtcdGroupService) GroupRemoveMember(ctx context.Context, groupName, uid string) error {
	if uid == "" {
		return constants.ErrEmptyUID
	}
	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	etcdRes, err := clientInstance.Txn(ctxT).
		If(clientv3.Compare(clientv3.CreateRevision(memberKey(groupName, uid)), ">", 0)).
		Then(clientv3.OpDelete(memberKey(groupName, uid))).
		Commit()

	if err != nil {
		return err
	}
	if !etcdRes.Succeeded {
		return constants.ErrMemberNotFound
	}
	return nil
}

// GroupRemoveAll clears all UIDs in the group
func (c *EtcdGroupService) GroupRemoveAll(ctx context.Context, groupName string) error {
	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	etcdRes, err := clientInstance.Txn(ctxT).
		If(clientv3.Compare(clientv3.CreateRevision(groupKey(groupName)), ">", 0)).
		Then(clientv3.OpDelete(memberKey(groupName, ""), clientv3.WithPrefix())).
		Commit()

	if err != nil {
		return err
	}
	if !etcdRes.Succeeded {
		return constants.ErrGroupNotFound
	}
	return nil
}

// GroupDelete deletes the whole group, including members and base group
func (c *EtcdGroupService) GroupDelete(ctx context.Context, groupName string) error {
	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	etcdRes, err := clientInstance.Txn(ctxT).
		If(clientv3.Compare(clientv3.CreateRevision(groupKey(groupName)), ">", 0)).
		Then(clientv3.OpDelete(memberKey(groupName, ""), clientv3.WithPrefix()),
			clientv3.OpDelete(groupKey(groupName))).
		Commit()

	if err != nil {
		return err
	}
	if !etcdRes.Succeeded {
		return constants.ErrGroupNotFound
	}
	return nil
}

// GroupCountMembers get current member amount in group
func (c *EtcdGroupService) GroupCountMembers(ctx context.Context, groupName string) (int, error) {
	ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
	defer cancel()
	etcdRes, err := clientInstance.Get(ctxT, memberKey(groupName, ""), clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		return 0, err
	}
	return int(etcdRes.Count), nil
}

// GroupRenewTTL will renew ETCD lease TTL
func (c *EtcdGroupService) GroupRenewTTL(ctx context.Context, groupName string) error {
	kv, err := getGroupKV(ctx, groupName)
	if err != nil {
		return err
	}
	if kv.Lease != 0 {
		ctxT, cancel := context.WithTimeout(ctx, transactionTimeout)
		defer cancel()
		_, err = clientInstance.KeepAliveOnce(ctxT, clientv3.LeaseID(kv.Lease))
		return err
	}
	return constants.ErrEtcdLeaseNotFound
}
