package s3api

import (
	"bytes"
	"encoding/json"
	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3account"

	//"github.com/seaweedfs/seaweedfs/weed/s3api"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3err"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"math"
	"sync"
)

var loadBucketMetadataFromFiler = func(r *BucketRegistry, bucketName string) (*BucketMetaData, error) {
	entry, err := filer_pb.GetEntry(r.s3a, util.NewFullPath(r.s3a.option.BucketsPath, bucketName))
	if err != nil {
		return nil, err
	}

	return buildBucketMetadata(entry), nil
}

type BucketMetaData struct {
	_ struct{} `type:"structure"`

	Name string

	//By default, when another AWS account uploads an object to S3 bucket,
	//that account (the object writer) owns the object, has access to it, and
	//can grant other users access to it through ACLs. You can use Object Ownership
	//to change this default behavior so that ACLs are disabled and you, as the
	//bucket owner, automatically own every object in your bucket.
	ObjectOwnership string

	// Container for the bucket owner's display name and ID.
	Owner *s3.Owner `type:"structure"`

	// A list of grants for access controls.
	Acl []*s3.Grant `locationName:"AccessControlList" locationNameList:"Grant" type:"list"`
}

type BucketRegistry struct {
	metadataCache     map[string]*BucketMetaData
	metadataCacheLock sync.RWMutex

	notFound     map[string]struct{}
	notFoundLock sync.RWMutex
	s3a          *S3ApiServer
}

func NewBucketRegistry(s3a *S3ApiServer) *BucketRegistry {
	br := &BucketRegistry{
		metadataCache: make(map[string]*BucketMetaData),
		notFound:      make(map[string]struct{}),
		s3a:           s3a,
	}
	err := br.init()
	if err != nil {
		glog.Fatal("init bucket registry failed", err)
		return nil
	}
	return br
}

func (r *BucketRegistry) init() error {
	err := filer_pb.List(r.s3a, r.s3a.option.BucketsPath, "", func(entry *filer_pb.Entry, isLast bool) error {
		r.LoadBucketMetadata(entry)
		return nil
	}, "", false, math.MaxUint32)
	return err
}

func (r *BucketRegistry) LoadBucketMetadata(entry *filer_pb.Entry) {
	bucketMetadata := buildBucketMetadata(entry)
	r.metadataCacheLock.Lock()
	defer r.metadataCacheLock.Unlock()
	r.metadataCache[entry.Name] = bucketMetadata
}

func buildBucketMetadata(entry *filer_pb.Entry) *BucketMetaData {
	entryJson, _ := json.Marshal(entry)
	glog.V(3).Infof("build bucket metadata,entry=%s", entryJson)
	bucketMetadata := &BucketMetaData{
		Name: entry.Name,

		//Default ownership: OwnershipBucketOwnerEnforced, which means Acl is disabled
		ObjectOwnership: s3_constants.OwnershipBucketOwnerEnforced,

		// Default owner: `AccountAdmin`
		Owner: &s3.Owner{
			ID:          &s3account.AccountAdmin.Id,
			DisplayName: &s3account.AccountAdmin.Name,
		},
	}
	if entry.Extended != nil {
		//ownership control
		ownership, ok := entry.Extended[s3_constants.ExtOwnershipKey]
		if ok {
			ownership := string(ownership)
			valid := s3_constants.ValidateOwnership(ownership)
			if valid {
				bucketMetadata.ObjectOwnership = ownership
			} else {
				glog.Warningf("Invalid ownership: %s, bucket: %s", ownership, bucketMetadata.Name)
			}
		}

		//access control policy
		acpBytes, ok := entry.Extended[s3_constants.ExtAcpKey]
		if ok {
			var acp s3.AccessControlPolicy
			err := jsonutil.UnmarshalJSON(&acp, bytes.NewReader(acpBytes))
			if err == nil {
				//validate owner
				if acp.Owner != nil && acp.Owner.ID != nil {
					bucketMetadata.Owner = acp.Owner
				} else {
					glog.Warningf("bucket ownerId is empty! bucket: %s", bucketMetadata.Name)
				}

				//acl
				bucketMetadata.Acl = acp.Grants
			} else {
				glog.Warningf("Unmarshal ACP: %s(%v), bucket: %s", string(acpBytes), err, bucketMetadata.Name)
			}
		}
	}
	return bucketMetadata
}

func (r *BucketRegistry) RemoveBucketMetadata(entry *filer_pb.Entry) {
	r.removeMetadataCache(entry.Name)
	r.unMarkNotFound(entry.Name)
}

func (r *BucketRegistry) GetBucketMetadata(bucketName string) (*BucketMetaData, s3err.ErrorCode) {
	r.metadataCacheLock.RLock()
	bucketMetadata, ok := r.metadataCache[bucketName]
	r.metadataCacheLock.RUnlock()
	if ok {
		return bucketMetadata, s3err.ErrNone
	}

	r.notFoundLock.RLock()
	_, ok = r.notFound[bucketName]
	r.notFoundLock.RUnlock()
	if ok {
		return nil, s3err.ErrNoSuchBucket
	}

	bucketMetadata, errCode := r.LoadBucketMetadataFromFiler(bucketName)
	if errCode != s3err.ErrNone {
		return nil, errCode
	}

	r.setMetadataCache(bucketMetadata)
	r.unMarkNotFound(bucketName)
	return bucketMetadata, s3err.ErrNone
}

func (r *BucketRegistry) LoadBucketMetadataFromFiler(bucketName string) (*BucketMetaData, s3err.ErrorCode) {
	r.notFoundLock.Lock()
	defer r.notFoundLock.Unlock()

	//check if already exists
	r.metadataCacheLock.RLock()
	bucketMetaData, ok := r.metadataCache[bucketName]
	r.metadataCacheLock.RUnlock()
	if ok {
		return bucketMetaData, s3err.ErrNone
	}

	//if not exists, load from filer
	bucketMetadata, err := loadBucketMetadataFromFiler(r, bucketName)
	if err != nil {
		if err == filer_pb.ErrNotFound {
			// The bucket doesn't actually exist and should no longer loaded from the filer
			r.notFound[bucketName] = struct{}{}
			return nil, s3err.ErrNoSuchBucket
		}
		return nil, s3err.ErrInternalError
	}
	return bucketMetadata, s3err.ErrNone
}

func (r *BucketRegistry) setMetadataCache(metadata *BucketMetaData) {
	r.metadataCacheLock.Lock()
	defer r.metadataCacheLock.Unlock()
	r.metadataCache[metadata.Name] = metadata
}

func (r *BucketRegistry) removeMetadataCache(bucket string) {
	r.metadataCacheLock.Lock()
	defer r.metadataCacheLock.Unlock()
	delete(r.metadataCache, bucket)
}

func (r *BucketRegistry) markNotFound(bucket string) {
	r.notFoundLock.Lock()
	defer r.notFoundLock.Unlock()
	r.notFound[bucket] = struct{}{}
}

func (r *BucketRegistry) unMarkNotFound(bucket string) {
	r.notFoundLock.Lock()
	defer r.notFoundLock.Unlock()
	delete(r.notFound, bucket)
}
