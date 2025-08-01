package ocm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/gardenlinux/glci/internal/cloudprovider"
	"github.com/gardenlinux/glci/internal/gl"
	"github.com/gardenlinux/glci/internal/log"
)

// ComponentDescriptor is an OCM data structure that Gardener consumes.
type ComponentDescriptor struct {
	Meta      componentDescriptorMetadata  `json:"meta"`
	Component componentDescriptorComponent `json:"component"`
}

// ToYAML serializes a component desctiptor to YAML.
func (d *ComponentDescriptor) ToYAML() ([]byte, error) {
	//nolint:wrapcheck // Directly wraps the YAML unmarshaling.
	return yaml.Marshal(d)
}

// BuildComponentDescriptor generates a component desciptor that includes all data except the results of the publishing process.
func BuildComponentDescriptor(ctx context.Context, source cloudprovider.ArtifactSource, publications []cloudprovider.Publication,
	ocmTarget cloudprovider.OCMTarget, aliases map[string][]string, version, commit string,
) (*ComponentDescriptor, error) {
	log.Debug(ctx, "Building component descriptor")

	baseURL, subPath := baseAndSub(ocmTarget.OCMRepository())

	descriptor := &ComponentDescriptor{
		Meta: componentDescriptorMetadata{
			ConfiguredVersion: "v2",
		},
		Component: componentDescriptorComponent{
			Name:    gl.GardenLinuxRepo,
			Version: version,
			Provider: componentDescriptorProvider{
				Name: componentProvider,
			},
			RepositoryContexts: []componentDescriptorRepositoryContext{
				{
					Type:                 "OCIRegistry",
					ComponentNameMapping: "urlPath",
					BaseURL:              baseURL,
					SubPath:              subPath,
				},
			},
			Sources: []componentDescriptorSource{
				{
					Name:    "gardenlinux",
					Version: version,
					Labels: []componentDescriptorlabel{
						{
							Name: "cloud.gardener.cnudie/dso/scanning-hints/source_analysis/v1",
							Value: map[string]any{
								"policy":  "skip",
								"comment": "repo only contains build instructions, source in this repo will not get incorporated into the final artifact",
							},
						},
					},
					Type: "git",
					Access: componentDescriptorGitHub{
						Type:    "gitHub",
						RepoURL: githubRepoURL,
						Commit:  commit,
					},
				},
			},
			ComponentReferences: []struct{}{},
		},
	}

	for _, publication := range publications {
		packages, err := getPackages(ctx, source, publication.Manifest)
		if err != nil {
			return nil, fmt.Errorf("cannot list packages for %s: %w", publication.Cname, err)
		}

		var imagePath gl.S3ReleaseFile
		imagePath, err = publication.Manifest.PathBySuffix(publication.Target.ImageSuffix())
		if err != nil {
			return nil, fmt.Errorf("missing image for %s: %w", publication.Cname, err)
		}

		var rootfsPath gl.S3ReleaseFile
		rootfsPath, err = publication.Manifest.PathBySuffix(".tar")
		if err != nil {
			return nil, fmt.Errorf("missing rootfs for %s: %w", publication.Cname, err)
		}

		labels := append(make([]componentDescriptorlabel, 0, 3), componentDescriptorlabel{
			Name: "gardener.cloud/gardenlinux/ci/build-metadata",
			Value: map[string]any{
				"modifiers":      publication.Manifest.Modifiers,
				"buildTimestamp": publication.Manifest.BuildTimestamp,
			},
		})

		packageVersions := getPackageVersions(packages, aliases)
		if len(packageVersions) > 0 {
			labels = append(labels, componentDescriptorlabel{
				Name:  "cloud.cnudie/dso/scanning-hints/package-versions",
				Value: packageVersions,
			})
		}

		descriptor.Component.Resources = append(descriptor.Component.Resources, componentDesciptorResource{
			Name:    "gardenlinux",
			Version: publication.Manifest.Version,
			ExtraIdentity: map[string]string{
				"feature-flags": strings.Join(publication.Manifest.Modifiers, ","),
				"architecture":  string(publication.Manifest.Architecture),
				"platform":      publication.Manifest.Platform,
			},
			Labels: labels,
			Type:   "virtual_machine_image",
			Digest: &componentDescriptorDigest{
				HashAlgorithm:          "NO-DIGEST",
				NormalisationAlgorithm: "EXCLUDE-FROM-SIGNATURE",
				Value:                  "NO-DIGEST",
			},
			Access: componentDescriptorS3{
				Type:   "s3",
				Bucket: publication.Manifest.S3Bucket,
				Key:    imagePath.S3Key,
			},
		}, componentDesciptorResource{
			Name:    "rootfs",
			Version: publication.Manifest.Version,
			ExtraIdentity: map[string]string{
				"feature-flags": strings.Join(publication.Manifest.Modifiers, ","),
				"architecture":  string(publication.Manifest.Architecture),
				"platform":      publication.Manifest.Platform,
			},
			Labels: []componentDescriptorlabel{
				{
					Name: "gardener.cloud/gardenlinux/ci/build-metadata",
					Value: map[string]any{
						"modifiers":      publication.Manifest.Modifiers,
						"buildTimestamp": publication.Manifest.BuildTimestamp,
						"debianPackages": getPackageList(packages),
					},
				},
				{
					Name: "cloud.gardener.cnudie/responsibles",
					Value: []map[string]string{
						{
							"type":  "emailAddress",
							"email": "andre.russ@sap.com",
						},
						{
							"type":  "emailAddress",
							"email": "v.riesop@sap.com",
						},
					},
				},
			},
			Type: "application/tar+vm-image-rootfs",
			Digest: &componentDescriptorDigest{
				HashAlgorithm:          "NO-DIGEST",
				NormalisationAlgorithm: "EXCLUDE-FROM-SIGNATURE",
				Value:                  "NO-DIGEST",
			},
			Access: componentDescriptorS3{
				Type:   "s3",
				Bucket: publication.Manifest.S3Bucket,
				Key:    rootfsPath.S3Key,
			},
		},
		)
	}

	return descriptor, nil
}

// AddPublicationOutput adds the outputs of the publishing process to an existing component descriptor.
func AddPublicationOutput(descriptor *ComponentDescriptor, publications []cloudprovider.Publication) error {
	if len(descriptor.Component.Resources) != len(publications)*2 {
		return fmt.Errorf("invalid component descriptor: expected %d resources, got %d", len(publications)*2,
			len(descriptor.Component.Resources))
	}

	for i, publication := range publications {
		if descriptor.Component.Resources[i*2].Type != "virtual_machine_image" {
			return fmt.Errorf("invalid component descriptor: resource %d has incorrect type %s", i*2,
				descriptor.Component.Resources[i*2].Type)
		}
		if descriptor.Component.Resources[i*2].Name != "gardenlinux" {
			return fmt.Errorf("invalid component descriptor: resource %d has incorrect name %s", i*2,
				descriptor.Component.Resources[i*2].Name)
		}

		descriptor.Component.Resources[i*2].Labels = append(descriptor.Component.Resources[i*2].Labels, componentDescriptorlabel{
			Name:  "gardener.cloud/gardenlinux/ci/published-image-metadata",
			Value: publication.Output,
		})
	}

	return nil
}

const (
	componentProvider = "sap-se"
	githubRepoURL     = "https://" + gl.GardenLinuxRepo
)

type componentDescriptorMetadata struct {
	ConfiguredVersion string `json:"configuredSchemaVersion"`
}

type componentDescriptorComponent struct {
	Name                string                                 `json:"name"`
	Version             string                                 `json:"version"`
	Provider            componentDescriptorProvider            `json:"provider"`
	RepositoryContexts  []componentDescriptorRepositoryContext `json:"repositoryContexts"`
	Sources             []componentDescriptorSource            `json:"sources"`
	ComponentReferences []struct{}                             `json:"componentReferences"`
	Resources           []componentDesciptorResource           `json:"resources"`
}

type componentDescriptorProvider struct {
	Name string `json:"name"`
}

type componentDescriptorRepositoryContext struct {
	Type                 string `json:"type"`
	ComponentNameMapping string `json:"componentNameMapping"`
	BaseURL              string `json:"baseUrl"`
	SubPath              string `json:"subPath"`
}

type componentDescriptorSource struct {
	Name    string                     `json:"name"`
	Version string                     `json:"version"`
	Labels  []componentDescriptorlabel `json:"labels,omitempty"`
	Type    string                     `json:"type"`
	Access  componentDescriptorGitHub  `json:"access"`
}

type componentDescriptorlabel struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

type componentDescriptorGitHub struct {
	Type    string `json:"type"`
	RepoURL string `json:"repoUrl"`
	Commit  string `json:"commit"`
}

type componentDesciptorResource struct {
	Name          string                     `json:"name"`
	Version       string                     `json:"version"`
	ExtraIdentity map[string]string          `json:"extraIdentity,omitempty"`
	Labels        []componentDescriptorlabel `json:"labels,omitempty"`
	Type          string                     `json:"type"`
	Digest        *componentDescriptorDigest `json:"digest,omitempty"`
	Access        componentDescriptorS3      `json:"access"`
}

type componentDescriptorDigest struct {
	HashAlgorithm          string `json:"hashAlgorithm"`
	NormalisationAlgorithm string `json:"normalisationAlgorithm"`
	Value                  string `json:"value"`
}

type componentDescriptorS3 struct {
	Type   string `json:"type"`
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

type nameVersion struct {
	name    string
	version string
}

func baseAndSub(repo string) (string, string) {
	var scheme, sub string
	base := repo
	i := strings.Index(base, "://")
	if i >= 0 {
		scheme = base[:i+3]
		base = base[i+3:]
	}
	i = strings.Index(base, "/")
	if i >= 0 {
		sub = base[i+1:]
		base = scheme + base[:i]
	}
	return base, sub
}

func getPackages(ctx context.Context, source cloudprovider.ArtifactSource, manifest *gl.Manifest) ([]nameVersion, error) {
	log.Debug(ctx, "Getting packages")

	manifestPath, err := manifest.PathBySuffix(".manifest")
	if err != nil {
		return nil, fmt.Errorf("missing packagae manifest: %w", err)
	}

	var obj io.ReadCloser
	obj, err = source.GetObject(ctx, manifestPath.S3Key)
	if err != nil {
		return nil, fmt.Errorf("cannot get package manifest: %w", err)
	}
	defer func() {
		_ = obj.Close()
	}()

	var packages []nameVersion
	scanner := bufio.NewScanner(obj)
	for scanner.Scan() {
		line := scanner.Text()
		tokens := strings.Fields(line)
		if len(tokens) == 0 {
			continue
		} else if len(tokens) != 2 {
			return nil, fmt.Errorf("invalid package-version %s", line)
		}

		packages = append(packages, nameVersion{
			name:    tokens[0],
			version: tokens[1],
		})
	}
	err = scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("cannot read package manifest: %w", err)
	}

	err = obj.Close()
	if err != nil {
		return nil, fmt.Errorf("cannot close package manifest: %w", err)
	}

	return packages, nil
}

func getPackageVersions(packages []nameVersion, aliases map[string][]string) []map[string]any {
	packageVersions := make([]map[string]any, 0, len(packages))
	for _, p := range packages {
		a, ok := aliases[p.name]
		if !ok {
			a = []string{}
		}
		packageVersions = append(packageVersions, map[string]any{
			"name":    p.name,
			"aliases": a,
			"version": p.version,
		})
	}
	return packageVersions
}

func getPackageList(packages []nameVersion) []string {
	l := make([]string, 0, len(packages))
	for _, p := range packages {
		l = append(l, fmt.Sprintf("%s %s", p.name, p.version))
	}
	return l
}
