package image

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/replicate/cog/pkg/config"
	"github.com/replicate/cog/pkg/docker"
	"github.com/replicate/cog/pkg/docker/command"
	"github.com/replicate/cog/pkg/dockercontext"
	"github.com/replicate/cog/pkg/dockerfile"
	"github.com/replicate/cog/pkg/dockerignore"
	"github.com/replicate/cog/pkg/global"
	"github.com/replicate/cog/pkg/util/console"
	"github.com/replicate/cog/pkg/weights"
)

const dockerignoreBackupPath = ".dockerignore.cog.bak"
const weightsManifestPath = ".cog/cache/weights_manifest.json"
const bundledSchemaFile = ".cog/openapi_schema.json"
const bundledSchemaPy = ".cog/schema.py"

var errGit = errors.New("git error")

// Build a Cog model from a config
//
// This is separated out from docker.Build(), so that can be as close as possible to the behavior of 'docker build'.
func Build(cfg *config.Config, dir, imageName string, secrets []string, noCache, separateWeights bool, useCudaBaseImage string, progressOutput string, schemaFile string, dockerfileFile string, useCogBaseImage *bool, strip bool, precompile bool, fastFlag bool, annotations map[string]string, localImage bool) error {
	console.Infof("Building Docker image from environment in cog.yaml as %s...", imageName)
	if fastFlag {
		console.Info("Fast build enabled.")
	}

	// remove bundled schema files that may be left from previous builds
	_ = os.Remove(bundledSchemaFile)
	_ = os.Remove(bundledSchemaPy)

	if err := checkCompatibleDockerIgnore(dir); err != nil {
		return err
	}

	var cogBaseImageName string

	if dockerfileFile != "" {
		dockerfileContents, err := os.ReadFile(dockerfileFile)
		if err != nil {
			return fmt.Errorf("Failed to read Dockerfile at %s: %w", dockerfileFile, err)
		}
		if err := docker.Build(dir, string(dockerfileContents), imageName, secrets, noCache, progressOutput, config.BuildSourceEpochTimestamp, dockercontext.StandardBuildDirectory, nil); err != nil {
			return fmt.Errorf("Failed to build Docker image: %w", err)
		}
	} else {
		command := docker.NewDockerCommand()
		generator, err := dockerfile.NewGenerator(cfg, dir, fastFlag, command, localImage)
		if err != nil {
			return fmt.Errorf("Error creating Dockerfile generator: %w", err)
		}
		contextDir, err := generator.BuildDir()
		if err != nil {
			return err
		}
		buildContexts, err := generator.BuildContexts()
		if err != nil {
			return err
		}
		defer func() {
			if err := generator.Cleanup(); err != nil {
				console.Warnf("Error cleaning up Dockerfile generator: %s", err)
			}
		}()
		generator.SetStrip(strip)
		generator.SetPrecompile(precompile)
		generator.SetUseCudaBaseImage(useCudaBaseImage)
		if useCogBaseImage != nil {
			generator.SetUseCogBaseImage(*useCogBaseImage)
		}

		if generator.IsUsingCogBaseImage() {
			cogBaseImageName, err = generator.BaseImage()
			if err != nil {
				return fmt.Errorf("Failed to get cog base image name: %s", err)
			}
		}

		if separateWeights {
			weightsDockerfile, runnerDockerfile, dockerignore, err := generator.GenerateModelBaseWithSeparateWeights(imageName)
			if err != nil {
				return fmt.Errorf("Failed to generate Dockerfile: %w", err)
			}

			if err := backupDockerignore(); err != nil {
				return fmt.Errorf("Failed to backup .dockerignore file: %w", err)
			}

			weightsManifest, err := generator.GenerateWeightsManifest()
			if err != nil {
				return fmt.Errorf("Failed to generate weights manifest: %w", err)
			}
			cachedManifest, _ := weights.LoadManifest(weightsManifestPath)
			changed := cachedManifest == nil || !weightsManifest.Equal(cachedManifest)
			if changed {
				if err := buildWeightsImage(dir, weightsDockerfile, imageName+"-weights", secrets, noCache, progressOutput, contextDir, buildContexts); err != nil {
					return fmt.Errorf("Failed to build model weights Docker image: %w", err)
				}
				err := weightsManifest.Save(weightsManifestPath)
				if err != nil {
					return fmt.Errorf("Failed to save weights hash: %w", err)
				}
			} else {
				console.Info("Weights unchanged, skip rebuilding and use cached image...")
			}

			if err := buildRunnerImage(dir, runnerDockerfile, dockerignore, imageName, secrets, noCache, progressOutput, contextDir, buildContexts); err != nil {
				return fmt.Errorf("Failed to build runner Docker image: %w", err)
			}
		} else {
			dockerfileContents, err := generator.GenerateDockerfileWithoutSeparateWeights()
			if err != nil {
				return fmt.Errorf("Failed to generate Dockerfile: %w", err)
			}
			if err := docker.Build(dir, dockerfileContents, imageName, secrets, noCache, progressOutput, config.BuildSourceEpochTimestamp, contextDir, buildContexts); err != nil {
				return fmt.Errorf("Failed to build Docker image: %w", err)
			}
		}
	}

	var schemaJSON []byte
	if schemaFile != "" {
		console.Infof("Validating model schema from %s...", schemaFile)
		data, err := os.ReadFile(schemaFile)
		if err != nil {
			return fmt.Errorf("Failed to read schema file: %w", err)
		}

		schemaJSON = data
	} else {
		console.Info("Validating model schema...")
		schema, err := GenerateOpenAPISchema(imageName, cfg.Build.GPU)
		if err != nil {
			return fmt.Errorf("Failed to get type signature: %w", err)
		}

		data, err := json.Marshal(schema)
		if err != nil {
			return fmt.Errorf("Failed to convert type signature to JSON: %w", err)
		}

		schemaJSON = data
	}

	// save open_api schema file
	if err := os.WriteFile(bundledSchemaFile, schemaJSON, 0o644); err != nil {
		return fmt.Errorf("failed to store bundled schema file %s: %w", bundledSchemaFile, err)
	}

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromData(schemaJSON)
	if err != nil {
		return fmt.Errorf("Failed to load model schema JSON: %w", err)
	}
	err = doc.Validate(loader.Context)
	if err != nil {
		return fmt.Errorf("Model schema is invalid: %w\n\n%s", err, string(schemaJSON))
	}

	console.Info("Adding labels to image...")

	// We used to set the cog_version and config labels in Dockerfile, because we didn't require running the
	// built image to get those. But, the escaping of JSON inside a label inside a Dockerfile was gnarly, and
	// doesn't seem to be a problem here, so do it here instead.
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("Failed to convert config to JSON: %w", err)
	}

	pipFreeze, err := GeneratePipFreeze(imageName, fastFlag)
	if err != nil {
		return fmt.Errorf("Failed to generate pip freeze from image: %w", err)
	}

	labels := map[string]string{
		command.CogVersionLabelKey:               global.Version,
		command.CogConfigLabelKey:                string(bytes.TrimSpace(configJSON)),
		global.LabelNamespace + "openapi_schema": string(schemaJSON),
		global.LabelNamespace + "pip_freeze":     pipFreeze,
		// Mark the image as having an appropriate init entrypoint. We can use this
		// to decide how/if to shim the image.
		global.LabelNamespace + "has_init": "true",
	}

	if cogBaseImageName != "" {
		labels[global.LabelNamespace+"cog-base-image-name"] = cogBaseImageName

		ref, err := name.ParseReference(cogBaseImageName)
		if err != nil {
			return fmt.Errorf("Failed to parse cog base image reference: %w", err)
		}

		img, err := remote.Image(ref)
		if err != nil {
			return fmt.Errorf("Failed to fetch cog base image: %w", err)
		}

		layers, err := img.Layers()
		if err != nil {
			return fmt.Errorf("Failed to get layers for cog base image: %w", err)
		}

		if len(layers) == 0 {
			return fmt.Errorf("Cog base image has no layers: %s", cogBaseImageName)
		}

		lastLayerIndex := len(layers) - 1
		layerLayerDigest, err := layers[lastLayerIndex].DiffID()
		if err != nil {
			return fmt.Errorf("Failed to get last layer digest for cog base image: %w", err)
		}

		lastLayer := layerLayerDigest.String()
		console.Debugf("Last layer of the cog base image: %s", lastLayer)

		labels[global.LabelNamespace+"cog-base-image-last-layer-sha"] = lastLayer
		labels[global.LabelNamespace+"cog-base-image-last-layer-idx"] = fmt.Sprintf("%d", lastLayerIndex)
	}

	if commit, err := gitHead(dir); commit != "" && err == nil {
		labels["org.opencontainers.image.revision"] = commit
	} else {
		console.Info("Unable to determine Git commit")
	}

	if tag, err := gitTag(dir); tag != "" && err == nil {
		labels["org.opencontainers.image.version"] = tag
	} else {
		console.Info("Unable to determine Git tag")
	}

	for key, val := range annotations {
		labels[key] = val
	}

	if err := docker.BuildAddLabelsAndSchemaToImage(imageName, labels, bundledSchemaFile, bundledSchemaPy); err != nil {
		return fmt.Errorf("Failed to add labels to image: %w", err)
	}
	return nil
}

func BuildBase(cfg *config.Config, dir string, useCudaBaseImage string, useCogBaseImage *bool, progressOutput string) (string, error) {
	// TODO: better image management so we don't eat up disk space
	// https://github.com/replicate/cog/issues/80
	imageName := config.BaseDockerImageName(dir)

	console.Info("Building Docker image from environment in cog.yaml...")
	command := docker.NewDockerCommand()
	generator, err := dockerfile.NewGenerator(cfg, dir, false, command, false)
	if err != nil {
		return "", fmt.Errorf("Error creating Dockerfile generator: %w", err)
	}
	contextDir, err := generator.BuildDir()
	if err != nil {
		return "", err
	}
	buildContexts, err := generator.BuildContexts()
	if err != nil {
		return "", err
	}
	defer func() {
		if err := generator.Cleanup(); err != nil {
			console.Warnf("Error cleaning up Dockerfile generator: %s", err)
		}
	}()

	generator.SetUseCudaBaseImage(useCudaBaseImage)
	if useCogBaseImage != nil {
		generator.SetUseCogBaseImage(*useCogBaseImage)
	}

	dockerfileContents, err := generator.GenerateModelBase()
	if err != nil {
		return "", fmt.Errorf("Failed to generate Dockerfile: %w", err)
	}
	if err := docker.Build(dir, dockerfileContents, imageName, []string{}, false, progressOutput, config.BuildSourceEpochTimestamp, contextDir, buildContexts); err != nil {
		return "", fmt.Errorf("Failed to build Docker image: %w", err)
	}
	return imageName, nil
}

func isGitWorkTree(dir string) bool {
	ctx, cancel := context.WithTimeout(context.TODO(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(out)) == "true"
}

func gitHead(dir string) (string, error) {
	if v, ok := os.LookupEnv("GITHUB_SHA"); ok && v != "" {
		return v, nil
	}

	if isGitWorkTree(dir) {
		ctx, cancel := context.WithTimeout(context.TODO(), 3*time.Second)
		defer cancel()

		out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			return "", err
		}

		return string(bytes.TrimSpace(out)), nil
	}

	return "", fmt.Errorf("Failed to find HEAD commit: %w", errGit)
}

func gitTag(dir string) (string, error) {
	if v, ok := os.LookupEnv("GITHUB_REF_NAME"); ok && v != "" {
		return v, nil
	}

	if isGitWorkTree(dir) {
		ctx, cancel := context.WithTimeout(context.TODO(), 3*time.Second)
		defer cancel()

		out, err := exec.CommandContext(ctx, "git", "-C", dir, "describe", "--tags", "--dirty").Output()
		if err != nil {
			return "", err
		}

		return string(bytes.TrimSpace(out)), nil
	}

	return "", fmt.Errorf("Failed to find ref name: %w", errGit)
}

func buildWeightsImage(dir, dockerfileContents, imageName string, secrets []string, noCache bool, progressOutput string, contextDir string, buildContexts map[string]string) error {
	if err := makeDockerignoreForWeightsImage(); err != nil {
		return fmt.Errorf("Failed to create .dockerignore file: %w", err)
	}
	if err := docker.Build(dir, dockerfileContents, imageName, secrets, noCache, progressOutput, config.BuildSourceEpochTimestamp, contextDir, buildContexts); err != nil {
		return fmt.Errorf("Failed to build Docker image for model weights: %w", err)
	}
	return nil
}

func buildRunnerImage(dir, dockerfileContents, dockerignoreContents, imageName string, secrets []string, noCache bool, progressOutput string, contextDir string, buildContexts map[string]string) error {
	if err := writeDockerignore(dockerignoreContents); err != nil {
		return fmt.Errorf("Failed to write .dockerignore file with weights included: %w", err)
	}
	if err := docker.Build(dir, dockerfileContents, imageName, secrets, noCache, progressOutput, config.BuildSourceEpochTimestamp, contextDir, buildContexts); err != nil {
		return fmt.Errorf("Failed to build Docker image: %w", err)
	}
	if err := restoreDockerignore(); err != nil {
		return fmt.Errorf("Failed to restore backup .dockerignore file: %w", err)
	}
	return nil
}

func makeDockerignoreForWeightsImage() error {
	if err := backupDockerignore(); err != nil {
		return fmt.Errorf("Failed to backup .dockerignore file: %w", err)
	}

	if err := writeDockerignore(dockerfile.DockerignoreHeader); err != nil {
		return fmt.Errorf("Failed to write .dockerignore file: %w", err)
	}
	return nil
}

func writeDockerignore(contents string) error {
	// read existing file contents from .dockerignore.cog.bak if it exists, and append to the new contents
	if _, err := os.Stat(dockerignoreBackupPath); err == nil {
		existingContents, err := os.ReadFile(dockerignoreBackupPath)
		if err != nil {
			return err
		}
		contents = string(existingContents) + "\n" + contents
	}

	return os.WriteFile(".dockerignore", []byte(contents), 0o644)
}

func backupDockerignore() error {
	if _, err := os.Stat(".dockerignore"); err != nil {
		if os.IsNotExist(err) {
			// .dockerignore file does not exist, nothing to backup
			return nil
		}
		return err
	}

	// rename the .dockerignore file to a new name
	return os.Rename(".dockerignore", dockerignoreBackupPath)
}

func restoreDockerignore() error {
	if err := os.Remove(".dockerignore"); err != nil {
		return err
	}

	if _, err := os.Stat(dockerignoreBackupPath); err != nil {
		if os.IsNotExist(err) {
			// .dockerignore backup file does not exist, nothing to restore
			return nil
		}
		return err
	}

	return os.Rename(dockerignoreBackupPath, ".dockerignore")
}

func checkCompatibleDockerIgnore(dir string) error {
	matcher, err := dockerignore.CreateMatcher(dir)
	if err != nil {
		return err
	}
	// If the matcher is nil and we don't have an error, we don't have a .dockerignore to scan.
	if matcher == nil {
		return nil
	}
	if matcher.MatchesPath(".cog") {
		return errors.New("The .cog tmp path cannot be ignored by docker in .dockerignore.")
	}
	return nil
}
