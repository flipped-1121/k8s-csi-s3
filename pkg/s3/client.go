package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"k8s.io/klog"
)

const (
	metadataName = ".metadata.json"
)

type s3Client struct {
	Config *Config
	minio  *minio.Client
	ctx    context.Context
}

// Config holds values to configure the driver
type Config struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	Endpoint        string
	Mounter         string
	Insecure        bool
	SignatureType   string
}

type FSMeta struct {
	BucketName    string   `json:"Name"`
	Prefix        string   `json:"Prefix"`
	Mounter       string   `json:"Mounter"`
	MountOptions  []string `json:"MountOptions"`
	CapacityBytes int64    `json:"CapacityBytes"`
}

func NewClient(cfg *Config) (*s3Client, error) {
	var client = &s3Client{}

	client.Config = cfg
	u, err := url.Parse(client.Config.Endpoint)
	if err != nil {
		return nil, err
	}
	ssl := u.Scheme == "https"
	endpoint := u.Hostname()
	if u.Port() != "" {
		endpoint = u.Hostname() + ":" + u.Port()
	}

	var transport = &http.Transport{}
	if client.Config.Insecure {
		tlsConfig := &tls.Config{}
		tlsConfig.InsecureSkipVerify = true
		transport.TLSClientConfig = tlsConfig
	}

	// Determine signature type
	var creds *credentials.Credentials
	signatureType := client.Config.SignatureType
	if signatureType == "" {
		signatureType = "V4"
	}

	switch signatureType {
	case "V2":
		creds = credentials.NewStatic(client.Config.AccessKeyID, client.Config.SecretAccessKey, "", credentials.SignatureV2)
	case "V4":
		creds = credentials.NewStatic(client.Config.AccessKeyID, client.Config.SecretAccessKey, "", credentials.SignatureV4)
	case "V4Streaming":
		creds = credentials.NewStatic(client.Config.AccessKeyID, client.Config.SecretAccessKey, "", credentials.SignatureV4Streaming)
	case "Anonymous":
		creds = credentials.NewStatic(client.Config.AccessKeyID, client.Config.SecretAccessKey, "", credentials.SignatureAnonymous)
	default:
		return nil, fmt.Errorf("invalid signature type: %s (supported: V2, V4, V4Streaming, Anonymous)", signatureType)
	}

	minioClient, err := minio.New(endpoint, &minio.Options{
		Transport: transport,
		Creds:     creds,
		Region:    client.Config.Region,
		Secure:    ssl,
	})
	if err != nil {
		return nil, err
	}
	client.minio = minioClient
	client.ctx = context.Background()
	klog.Infof("S3 client initialized - Endpoint: %s, Region: %s, SignatureType: %s, Insecure: %v",
		client.Config.Endpoint, client.Config.Region, client.Config.SignatureType, client.Config.Insecure)
	return client, nil
}

func NewClientFromSecret(secret map[string]string) (*s3Client, error) {
	insecure, _ := strconv.ParseBool(secret["insecure"])
	return NewClient(&Config{
		AccessKeyID:     secret["accessKeyID"],
		SecretAccessKey: secret["secretAccessKey"],
		Region:          secret["region"],
		Endpoint:        secret["endpoint"],
		// Mounter is set in the volume preferences, not secrets
		Mounter:       "",
		Insecure:      insecure,
		SignatureType: secret["signatureType"],
	})
}

func (client *s3Client) BucketExists(bucketName string) (bool, error) {
	klog.V(4).Infof("Checking if bucket exists: %s", bucketName)
	exists, err := client.minio.BucketExists(client.ctx, bucketName)
	if err != nil {
		klog.Errorf("Error checking bucket existence for %s: %v", bucketName, err)
	} else {
		klog.V(4).Infof("Bucket %s exists: %v", bucketName, exists)
	}
	return exists, err
}

func (client *s3Client) CreateBucket(bucketName string) error {
	klog.Infof("Creating bucket: %s with region: %s", bucketName, client.Config.Region)
	err := client.minio.MakeBucket(client.ctx, bucketName, minio.MakeBucketOptions{Region: client.Config.Region})
	if err != nil {
		klog.Errorf("Failed to create bucket %s: %v", bucketName, err)
	} else {
		klog.Infof("Successfully created bucket: %s", bucketName)
	}
	return err
}

func (client *s3Client) CreatePrefix(bucketName string, prefix string) error {
	if prefix != "" {
		klog.V(4).Infof("Creating prefix: %s in bucket: %s, client config: %v", prefix, bucketName, client.Config)
		_, err := client.minio.PutObject(client.ctx, bucketName, prefix+"/", bytes.NewReader([]byte("")), 0, minio.PutObjectOptions{})
		if err != nil {
			klog.Errorf("Failed to create prefix %s in bucket %s: %v", prefix, bucketName, err)
			return err
		}
		klog.V(4).Infof("Successfully created prefix: %s", prefix)
	}
	return nil
}

func (client *s3Client) RemovePrefix(bucketName string, prefix string) error {
	var err error

	if err = client.removeObjects(bucketName, prefix); err == nil {
		return client.minio.RemoveObject(client.ctx, bucketName, prefix, minio.RemoveObjectOptions{})
	}

	klog.Warningf("removeObjects failed with: %s, will try removeObjectsOneByOne", err)

	if err = client.removeObjectsOneByOne(bucketName, prefix); err == nil {
		return client.minio.RemoveObject(client.ctx, bucketName, prefix, minio.RemoveObjectOptions{})
	}

	return err
}

func (client *s3Client) RemoveBucket(bucketName string) error {
	var err error

	if err = client.removeObjects(bucketName, ""); err == nil {
		return client.minio.RemoveBucket(client.ctx, bucketName)
	}

	klog.Warningf("removeObjects failed with: %s, will try removeObjectsOneByOne", err)

	if err = client.removeObjectsOneByOne(bucketName, ""); err == nil {
		return client.minio.RemoveBucket(client.ctx, bucketName)
	}

	return err
}

func (client *s3Client) removeObjects(bucketName, prefix string) error {
	objectsCh := make(chan minio.ObjectInfo)
	var listErr error

	go func() {
		defer close(objectsCh)

		for object := range client.minio.ListObjects(
			client.ctx,
			bucketName,
			minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if object.Err != nil {
				listErr = object.Err
				return
			}
			objectsCh <- object
		}
	}()

	if listErr != nil {
		klog.Error("Error listing objects", listErr)
		return listErr
	}

	select {
	default:
		opts := minio.RemoveObjectsOptions{
			GovernanceBypass: true,
		}
		errorCh := client.minio.RemoveObjects(client.ctx, bucketName, objectsCh, opts)
		haveErrWhenRemoveObjects := false
		for e := range errorCh {
			klog.Errorf("Failed to remove object %s, error: %s", e.ObjectName, e.Err)
			haveErrWhenRemoveObjects = true
		}
		if haveErrWhenRemoveObjects {
			return fmt.Errorf("Failed to remove all objects of bucket %s", bucketName)
		}
	}

	return nil
}

// will delete files one by one without file lock
func (client *s3Client) removeObjectsOneByOne(bucketName, prefix string) error {
	parallelism := 16
	objectsCh := make(chan minio.ObjectInfo, parallelism)
	guardCh := make(chan int, parallelism)
	var listErr error
	var totalObjects int64 = 0
	var removeErrors int64 = 0

	go func() {
		defer close(objectsCh)

		for object := range client.minio.ListObjects(client.ctx, bucketName,
			minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			if object.Err != nil {
				listErr = object.Err
				return
			}
			atomic.AddInt64(&totalObjects, 1)
			objectsCh <- object
		}
	}()

	if listErr != nil {
		klog.Error("Error listing objects", listErr)
		return listErr
	}

	for object := range objectsCh {
		guardCh <- 1
		go func(obj minio.ObjectInfo) {
			err := client.minio.RemoveObject(client.ctx, bucketName, obj.Key,
				minio.RemoveObjectOptions{VersionID: obj.VersionID})
			if err != nil {
				klog.Errorf("Failed to remove object %s, error: %s", obj.Key, err)
				atomic.AddInt64(&removeErrors, 1)
			}
			<-guardCh
		}(object)
	}
	for i := 0; i < parallelism; i++ {
		guardCh <- 1
	}
	for i := 0; i < parallelism; i++ {
		<-guardCh
	}

	if removeErrors > 0 {
		return fmt.Errorf("Failed to remove %v objects out of total %v of path %s", removeErrors, totalObjects, bucketName)
	}

	return nil
}
