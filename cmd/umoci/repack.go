/*
 * umoci: Umoci Modifies Open Containers' Images
 * Copyright (C) 2016, 2017 SUSE LLC.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/cyphar/umoci"
	"github.com/cyphar/umoci/mutate"
	"github.com/cyphar/umoci/oci/cas"
	igen "github.com/cyphar/umoci/oci/generate"
	"github.com/cyphar/umoci/oci/layer"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
	"github.com/vbatts/go-mtree"
	"golang.org/x/net/context"
)

var repackCommand = uxHistory(cli.Command{
	Name:  "repack",
	Usage: "repacks an OCI runtime bundle into a reference",
	ArgsUsage: `--image <image-path>[:<new-tag>] <bundle>

Where "<image-path>" is the path to the OCI image, "<new-tag>" is the name of
the tag that the new image will be saved as (if not specified, defaults to
"latest"), and "<bundle>" is the bundle from which to generate the required
layers.

The "<image-path>" MUST be the same image that was used to create "<bundle>"
(using umoci-unpack(1)). Otherwise umoci will not be able to modify the
original manifest to add the diff layer.

All uid-map and gid-map settings are automatically loaded from the bundle
metadata (which is generated by umoci-unpack(1)) so if you unpacked an image
using a particular mapping then the same mapping will be used to generate the
new layer.

It should be noted that this is not the same as oci-create-layer because it
uses go-mtree to create diff layers from runtime bundles unpacked with
umoci-unpack(1). In addition, it modifies the image so that all of the relevant
manifest and configuration information uses the new diff atop the old manifest.`,

	// repack creates a new image, with a given tag.
	Category: "image",

	Action: repack,

	Before: func(ctx *cli.Context) error {
		if ctx.NArg() != 1 {
			return errors.Errorf("invalid number of positional arguments: expected <bundle>")
		}
		if ctx.Args().First() == "" {
			return errors.Errorf("bundle path cannot be empty")
		}
		ctx.App.Metadata["bundle"] = ctx.Args().First()
		return nil
	},
})

func repack(ctx *cli.Context) error {
	imagePath := ctx.App.Metadata["--image-path"].(string)
	tagName := ctx.App.Metadata["--image-tag"].(string)
	bundlePath := ctx.App.Metadata["bundle"].(string)

	// Read the metadata first.
	meta, err := ReadBundleMeta(bundlePath)
	if err != nil {
		return errors.Wrap(err, "read umoci.json metadata")
	}

	logrus.WithFields(logrus.Fields{
		"version":     meta.Version,
		"from":        meta.From,
		"map_options": meta.MapOptions,
	}).Debugf("umoci: loaded UmociMeta metadata")

	// FIXME: Implement support for manifest lists.
	if meta.From.MediaType != ispec.MediaTypeImageManifest {
		return errors.Wrap(fmt.Errorf("descriptor does not point to ispec.MediaTypeImageManifest: not implemented: %s", meta.From.MediaType), "invalid saved from descriptor")
	}

	// Get a reference to the CAS.
	engine, err := cas.Open(imagePath)
	if err != nil {
		return errors.Wrap(err, "open CAS")
	}
	defer engine.Close()

	// Create the mutator.
	mutator, err := mutate.New(engine, meta.From)
	if err != nil {
		return errors.Wrap(err, "create mutator for base image")
	}

	mtreeName := strings.Replace(meta.From.Digest, "sha256:", "sha256_", 1)
	mtreePath := filepath.Join(bundlePath, mtreeName+".mtree")
	fullRootfsPath := filepath.Join(bundlePath, layer.RootfsName)

	logrus.WithFields(logrus.Fields{
		"image":  imagePath,
		"bundle": bundlePath,
		"rootfs": layer.RootfsName,
		"mtree":  mtreePath,
	}).Debugf("umoci: repacking OCI image")

	mfh, err := os.Open(mtreePath)
	if err != nil {
		return errors.Wrap(err, "open mtree")
	}
	defer mfh.Close()

	spec, err := mtree.ParseSpec(mfh)
	if err != nil {
		return errors.Wrap(err, "parse mtree")
	}

	logrus.WithFields(logrus.Fields{
		"keywords": MtreeKeywords,
	}).Debugf("umoci: parsed mtree spec")

	fsEval := umoci.DefaultFsEval
	if meta.MapOptions.Rootless {
		fsEval = umoci.RootlessFsEval
	}

	diffs, err := mtree.Check(fullRootfsPath, spec, MtreeKeywords, fsEval)
	if err != nil {
		return errors.Wrap(err, "check mtree")
	}

	logrus.WithFields(logrus.Fields{
		"ndiff": len(diffs),
	}).Debugf("umoci: checked mtree spec")

	reader, err := layer.GenerateLayer(fullRootfsPath, diffs, &meta.MapOptions)
	if err != nil {
		return errors.Wrap(err, "generate diff layer")
	}
	defer reader.Close()

	imageMeta, err := mutator.Meta(context.Background())
	if err != nil {
		return errors.Wrap(err, "get image metadata")
	}

	history := ispec.History{
		Author:     imageMeta.Author,
		Comment:    "",
		Created:    time.Now().Format(igen.ISO8601),
		CreatedBy:  "umoci config", // XXX: Should we append argv to this?
		EmptyLayer: false,
	}

	if val, ok := ctx.App.Metadata["--history.author"]; ok {
		history.Author = val.(string)
	}
	if val, ok := ctx.App.Metadata["--history.comment"]; ok {
		history.Comment = val.(string)
	}
	if val, ok := ctx.App.Metadata["--history.created"]; ok {
		history.Created = val.(string)
	}
	if val, ok := ctx.App.Metadata["--history.created_by"]; ok {
		history.CreatedBy = val.(string)
	}

	// TODO: We should add a flag to allow for a new layer to be made
	//       non-distributable.
	if err := mutator.Add(context.Background(), reader, history); err != nil {
		return errors.Wrap(err, "add diff layer")
	}

	newDescriptor, err := mutator.Commit(context.Background())
	if err != nil {
		return errors.Wrap(err, "commit mutated image")
	}

	logrus.WithFields(logrus.Fields{
		"mediatype": newDescriptor.MediaType,
		"digest":    newDescriptor.Digest,
		"size":      newDescriptor.Size,
	}).Infof("created new image")

	// We have to clobber the old reference.
	// XXX: Should we output some warning if we actually did remove an old
	//      reference?
	if err := engine.DeleteReference(context.Background(), tagName); err != nil {
		return err
	}
	if err := engine.PutReference(context.Background(), tagName, &newDescriptor); err != nil {
		return err
	}

	return nil
}
