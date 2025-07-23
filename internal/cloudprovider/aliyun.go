package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	"github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"

	"github.com/gardenlinux/glci/internal/gl"
	"github.com/gardenlinux/glci/internal/log"
	"github.com/gardenlinux/glci/internal/util"
)

func init() {
	util.CleanEnv("OSS_")

	registerPublishingTarget(func() PublishingTarget {
		return &aliyun{}
	})
}

func (*aliyun) Type() string {
	return "Aliyun"
}

func (p *aliyun) SetCredentials(creds map[string]any) error {
	return setCredentials(creds, "alicloud", &p.creds)
}

func (p *aliyun) SetTargetConfig(_ context.Context, cfg map[string]any, sources map[string]ArtifactSource) error {
	err := setConfig(cfg, &p.pubCfg)
	if err != nil {
		return err
	}

	if p.creds == nil {
		return errors.New("credentials not set")
	}

	_, ok := sources[p.pubCfg.Source]
	if !ok {
		return fmt.Errorf("unknown source %s", p.pubCfg.Source)
	}

	var creds aliyunCredentials
	creds, ok = p.creds[p.pubCfg.Config]
	if !ok {
		return fmt.Errorf("missing credentials config %s", p.pubCfg.Config)
	}

	p.ossClient = oss.NewClient(oss.LoadDefaultConfig().WithCredentialsProvider(credentials.NewStaticCredentialsProvider(creds.AccessKeyID,
		creds.AccessKeySecret)).WithRegion(creds.Region))

	p.ecsClient, err = client.NewClient(&utils.Config{
		AccessKeyId:     &creds.AccessKeyID,
		AccessKeySecret: &creds.AccessKeySecret,
		RegionId:        &creds.Region,
	})
	if err != nil {
		return fmt.Errorf("cannot create ecs client: %w", err)
	}

	return nil
}

func (*aliyun) Close() error {
	return nil
}

func (*aliyun) ImageSuffix() string {
	return ".qcow2"
}

func (p *aliyun) Publish(ctx context.Context, cname string, manifest *gl.Manifest, sources map[string]ArtifactSource) (PublishingOutput,
	error,
) {
	image := p.imageName(cname, manifest.Version, manifest.BuildCommittish)
	imagePath, err := manifest.PathBySuffix(p.ImageSuffix())
	if err != nil {
		return nil, fmt.Errorf("missing image: %w", err)
	}
	source := sources[p.pubCfg.Source]
	region := p.creds[p.pubCfg.Config].Region
	ctx = log.WithValues(ctx, "target", p.Type(), "image", image, "sourceType", source.Type(), "sourceRepo",
		source.Repository(), "region", region)

	var regions []string
	regions, err = p.listRegions(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list regions: %w", err)
	}
	if p.pubCfg.Regions != nil {
		regions = util.Subset(regions, *p.pubCfg.Regions)
	}
	if len(regions) == 0 {
		return nil, errors.New("no available regions")
	}

	var blob string
	blob, err = p.uploadBlob(ctx, source, imagePath.S3Key, image)
	if err != nil {
		return nil, fmt.Errorf("cannot upload blob for image %s: %w", image, err)
	}

	var imageID string
	imageID, err = p.importImage(ctx, blob, image)
	if err != nil {
		return nil, fmt.Errorf("cannot import image %s from blob %s: %w", image, blob, err)
	}
	ctx = log.WithValues(ctx, "imageID", imageID)

	var images map[string]string
	images, err = p.copyImage(ctx, image, imageID, region, regions)
	if err != nil {
		return nil, fmt.Errorf("cannot copy image %s: %w", image, err)
	}

	err = p.waitForImages(ctx, images)
	if err != nil {
		return nil, fmt.Errorf("cannot finalize images: %w", err)
	}

	err = p.makePublic(ctx, images)
	if err != nil {
		return nil, fmt.Errorf("cannot make images public: %w", err)
	}

	var output aliyunPublishingOutput
	for region, imageID = range images {
		output = append(output, aliyunPublishedImage{
			Region: region,
			ID:     imageID,
			Name:   image,
		})
	}

	return output, nil
}

func (p *aliyun) Remove(ctx context.Context, cname string, manifest *gl.Manifest, _ map[string]ArtifactSource) error {
	image := p.imageName(cname, manifest.Version, manifest.BuildCommittish)
	ctx = log.WithValues(ctx, "target", p.Type(), "image", image)

	regions, err := p.listRegions(ctx)
	if err != nil {
		return fmt.Errorf("cannot list regions: %w", err)
	}
	if len(regions) == 0 {
		return nil
	}

	var images map[string]string
	images, err = p.getImageIDsByRegion(ctx, image, regions)
	if err != nil {
		return fmt.Errorf("cannot get image IDs for image %s: %w", image, err)
	}

	for region, imageID := range images {
		err = p.deleteImage(ctx, imageID, region)
		if err != nil {
			return fmt.Errorf("cannot delete image %s: %w", image, err)
		}
	}

	return nil
}

type aliyun struct {
	creds     map[string]aliyunCredentials
	pubCfg    aliyunPublishingConfig
	ossClient *oss.Client
	ecsClient *client.Client
}

type aliyunCredentials struct {
	Region          string `mapstructure:"region"`
	AccessKeyID     string `mapstructure:"access_key_id"`
	AccessKeySecret string `mapstructure:"access_key_secret"`
}

type aliyunPublishingConfig struct {
	Source  string    `mapstructure:"source"`
	Config  string    `mapstructure:"config"`
	Bucket  string    `mapstructure:"bucket"`
	Regions *[]string `mapstructure:"regions,omitempty"`
}

type aliyunPublishingOutput []aliyunPublishedImage

type aliyunPublishedImage struct {
	Region string `mapstructure:"region"`
	ID     string `mapstructure:"id"`
	Name   string `mapstructure:"name"`
}

func (*aliyun) imageName(cname, version, committish string) string {
	return fmt.Sprintf("gardenlinux-%s-%s-%.8s", cname, version, committish)
}

func (p *aliyun) uploadBlob(ctx context.Context, source ArtifactSource, key, image string) (string, error) {
	ossKey := image + p.ImageSuffix()
	ctx = log.WithValues(ctx, "bucket", p.pubCfg.Bucket, "key", key, "ossKey", ossKey)

	obj, err := source.GetObject(ctx, key)
	if err != nil {
		return "", fmt.Errorf("cannot get blob: %w", err)
	}
	defer func() {
		_ = obj.Close()
	}()

	log.Info(ctx, "Uploading blob")
	_, err = p.ossClient.PutObject(ctx, &oss.PutObjectRequest{
		Bucket: &p.pubCfg.Bucket,
		Key:    &ossKey,
		Body:   obj,
	})
	if err != nil {
		return "", fmt.Errorf("cannot put object %s in bucket %s: %w", ossKey, p.pubCfg.Bucket, err)
	}
	log.Debug(ctx, "Blob uploaded")

	err = obj.Close()
	if err != nil {
		return "", fmt.Errorf("cannot close blob: %w", err)
	}

	return ossKey, nil
}

func (p *aliyun) listRegions(ctx context.Context) ([]string, error) {
	log.Debug(ctx, "Listing available regions")
	r, err := p.ecsClient.DescribeRegions(&client.DescribeRegionsRequest{})
	if err != nil {
		return nil, fmt.Errorf("cannot describe regions: %w", err)
	}
	if r.Body == nil {
		return nil, errors.New("cannot describe regions: missing body")
	}
	if r.Body.Regions == nil {
		return nil, errors.New("cannot describe regions: missing regions")
	}

	regions := make([]string, 0, len(r.Body.Regions.Region))
	for _, region := range r.Body.Regions.Region {
		if region == nil {
			return nil, errors.New("cannot describe regions: missing region")
		}
		if region.RegionId == nil {
			return nil, errors.New("cannot describe regions: missing region ID")
		}
		regions = append(regions, *region.RegionId)
	}

	return regions, nil
}

func (p *aliyun) importImage(ctx context.Context, blob, image string) (string, error) {
	region := p.creds[p.pubCfg.Config].Region
	ctx = log.WithValues(ctx, "blob", blob)

	log.Info(ctx, "Importing image")
	r, err := p.ecsClient.ImportImage(&client.ImportImageRequest{
		DiskDeviceMapping: []*client.ImportImageRequestDiskDeviceMapping{
			{
				DiskImageSize: util.Ptr(int32(20)),
				Format:        util.Ptr("qcow2"),
				OSSBucket:     &p.pubCfg.Bucket,
				OSSObject:     &blob,
			},
		},
		Features: &client.ImportImageRequestFeatures{
			NvmeSupport: util.Ptr("supported"),
		},
		ImageName: &image,
		RegionId:  &region,
	})
	if err != nil {
		return "", fmt.Errorf("cannot import image: %w", err)
	}
	if r.Body == nil {
		return "", errors.New("cannot import image: missing body")
	}
	if r.Body.ImageId == nil {
		return "", errors.New("cannot import image: missing image ID")
	}
	log.Info(ctx, "Image ready")

	return *r.Body.ImageId, nil
}

func (p *aliyun) copyImage(ctx context.Context, image, imageID, fromRegion string,
	toRegions []string,
) (map[string]string, error) {
	images := make(map[string]string, len(toRegions))

	for _, region := range toRegions {
		if region == fromRegion {
			images[region] = imageID
			continue
		}

		log.Info(ctx, "Copying image", "toRegion", region)
		r, err := p.ecsClient.CopyImage(&client.CopyImageRequest{
			DestinationImageName: &image,
			DestinationRegionId:  &region,
			ImageId:              &imageID,
			RegionId:             &fromRegion,
		})
		if err != nil {
			return images, fmt.Errorf("cannot copy image %s to region %s: %w", imageID, region, err)
		}
		if r.Body == nil {
			return nil, fmt.Errorf("cannot copy image %s to region %s: missing body", imageID, region)
		}
		if r.Body.ImageId == nil {
			return nil, fmt.Errorf("cannot copy image %s to region %s: missing image ID", imageID, region)
		}
		images[region] = *r.Body.ImageId
	}

	return images, nil
}

func (p *aliyun) waitForImages(ctx context.Context, images map[string]string) error {
	for region, imageID := range images {
		var status string
		for status != "Available" {
			log.Debug(ctx, "Waiting for image", "toRegion", region, "toImageID", imageID)
			r, err := p.ecsClient.DescribeImages(&client.DescribeImagesRequest{
				ImageId:  &imageID,
				RegionId: &region,
			})
			if err != nil {
				return fmt.Errorf("cannot get status of image %s in region %s: %w", imageID, region, err)
			}
			if r.Body == nil {
				return fmt.Errorf("cannot get status of image %s in region %s: missing body", imageID, region)
			}
			if r.Body.Images == nil || len(r.Body.Images.Image) != 1 {
				return fmt.Errorf("cannot get status of image %s in region %s: missing images", imageID, region)
			}
			if r.Body.Images.Image[0] == nil {
				return fmt.Errorf("cannot get status of image %s in region %s: missing image", imageID, region)
			}
			if r.Body.Images.Image[0].Status == nil {
				return fmt.Errorf("cannot get status of image %s in region %s: missing status", imageID, region)
			}
			status = *r.Body.Images.Image[0].Status

			if status != "Available" {
				if status != "Creating" {
					return fmt.Errorf("image %s in region %s has status %s", imageID, region, status)
				}

				time.Sleep(time.Second * 7)
			}
		}
	}
	log.Info(ctx, "Images ready", "count", len(images))

	return nil
}

func (p *aliyun) makePublic(ctx context.Context, images map[string]string) error {
	for region, imageID := range images {
		log.Debug(ctx, "Adding launch permission to image", "toRegion", region, "toImageID", imageID)
		_, err := p.ecsClient.ModifyImageSharePermission(&client.ModifyImageSharePermissionRequest{
			ImageId:  &imageID,
			IsPublic: util.Ptr(true),
			RegionId: &region,
		})
		if err != nil {
			return fmt.Errorf("cannot modify share permission of image %s in region %s: %w", imageID, region, err)
		}
	}

	return nil
}

func (p *aliyun) getImageIDsByRegion(ctx context.Context, image string, regions []string) (map[string]string, error) {
	images := make(map[string]string, len(regions))
	for _, region := range regions {
		log.Debug(ctx, "Getting image ID", "fromRegion", region)
		r, err := p.ecsClient.DescribeImages(&client.DescribeImagesRequest{
			ImageName: &image,
			RegionId:  &region,
		})
		if err != nil {
			return nil, fmt.Errorf("cannot describe image in region %s: %w", region, err)
		}
		if r.Body == nil {
			return nil, fmt.Errorf("cannot describe image in region %s: missing body", region)
		}
		if r.Body.Images == nil {
			return nil, fmt.Errorf("cannot describe image in region %s: missing images", region)
		}
		if len(r.Body.Images.Image) > 1 {
			return nil, fmt.Errorf("too many images with the same name in region %s", region)
		}
		if len(r.Body.Images.Image) < 1 {
			continue
		}
		if r.Body.Images.Image[0] == nil {
			return nil, fmt.Errorf("cannot describe image in region %s: missing image", region)
		}
		if r.Body.Images.Image[0].ImageId == nil {
			return nil, fmt.Errorf("cannot describe image in region %s: missing image ID", region)
		}
		images[region] = *r.Body.Images.Image[0].ImageId
	}

	return images, nil
}

func (p *aliyun) deleteImage(ctx context.Context, imageID, region string) error {
	ctx = log.WithValues(ctx, "fromRegion", region)

	// FIXME: does this delete the blob and/or the snapshot?
	log.Info(ctx, "Deleting image")
	_, err := p.ecsClient.DeleteImage(&client.DeleteImageRequest{
		ImageId:  &imageID,
		RegionId: &region,
	})
	if err != nil {
		return fmt.Errorf("cannot delete image %s in region %s: %w", imageID, region, err)
	}

	return nil
}
