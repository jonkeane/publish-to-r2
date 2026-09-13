# Lightroom Classic → R2 Publisher

A macOS Lightroom Classic Export/Publish Service, a Go uploader, and a Hugo integration for `jonkeane/photo-site`. Lightroom publishes immutable JPEGs and gallery manifests to R2. A small site importer reads those manifests before Hugo builds **one ordinary page per photograph**, preserving the existing pagination, photo navigation, photo strip, and taxonomies.

The target is Lightroom Classic 15.5 on Apple Silicon. The local installation reports 15.5.1.

## Build and install

Requires macOS, Xcode Command Line Tools, Go (the pinned Go 1.26.8 toolchain is downloaded automatically), and Lua for plugin tests.

```sh
make test
make package
```

Install the bundled `R2Publisher.lrplugin` through Lightroom's File → Plug-in Manager. In Publish Services, create a Cloudflare R2 service. Enter the R2 S3 endpoint, bucket, public base URL, access key ID, and secret access key. The catalog field starts with the active catalog. Click **Test Connection** and save the settings. 

The Cloudflare R2 settings include the destination, catalog, and credentials. **Storage and retention** controls the spool limit, maximum JPEG size, retained history count, and cleanup grace period.

## Website integration

The `site/` directory is a separate checkout of your existing photo site, with the integration changes ready for review. `make package` also creates `dist/photo-site-r2.patch`. No browser-side gallery reader is used.

1. Publish a collection, then right-click it and choose **Edit Collection**. Under **Cloudflare R2**, click **Copy Gallery ID**. The ID appears after the first successful publish. **Go to Published Collection** also opens its manifest URL: `https://YOUR_IMAGE_DOMAIN/galleries/GALLERY_ID/current.json`. The **Export gallery ID** setting appears only in ordinary Export, not Publish Service settings.
2. Add `r2_gallery_id: "GALLERY_ID"` to that gallery's existing `content/gallery/SLUG/_index.md`.
3. From the photo-site checkout, run:

   ```sh
   export R2_PUBLIC_BASE_URL="https://YOUR_IMAGE_DOMAIN"
   go run ./cmd/r2import
   hugo
   ```

4. Use your normal Netlify deployment workflow. To fetch new manifests during each build, prefix your existing Hugo build command with `go run ./cmd/r2import &&` and set the public `R2_PUBLIC_BASE_URL` in the build environment. Retain any existing Flickr/asset preparation steps.

The importer writes `data/r2/galleries/SLUG.json`. The existing content adapter maps R2 records to its photo parameters and continues to call `AddPage`. R2 takes precedence for a migrated gallery; other Flickr galleries continue working. Image edits keep the photo page's UUID-based path and update its R2 image URL. New photos/removals and changed metadata reach the website on the next import/build/deploy. The plugin never starts a Netlify build.

Read [setup and operations](docs/setup.md), [protocol and recovery](docs/protocol.md), [metadata mapping](docs/metadata.md), and [acceptance testing](docs/acceptance.md) before importing your full library.

### Cover-only gallery photos

In Lightroom, add the keyword **`gallery-cover`** to one photo in a published collection, with the keyword included on export. For a separate crop, create a virtual copy, crop it for the cover, and include both the original and the copy in the collection. Put `gallery-cover` only on the cover copy.

Publish the affected photos, then run the usual site import/build/deploy. The marked photo supplies the homepage cover, gallery listing cover, and gallery social previews, overriding the gallery's existing `image:` setting. It has no individual photo page and is excluded from gallery grids, pagination, navigation, photo strips, and taxonomies. The original remains a normal gallery photo.

## Project layout

- `R2Publisher.lrplugin/`: SDK callbacks, identity metadata, public metadata extraction, and helper bridge.
- `uploader/`: pinned AWS SDK for Go v2 transport, local credentials, transaction journal, diagnostics, reconciliation, and reviewed cleanup.
- `schemas/`: version 1 job, result, and public-manifest JSON schemas.
- `tests/`: Lua SDK mocks, schema checks, and Hugo integration tests. Go transaction/transport tests live beside their packages.

Runtime state lives under `~/Library/Application Support/R2Publisher` with private permissions. Use `R2PUBLISHER_HOME` to relocate runtime state, including saved credentials. Defaults: 5 GiB spool, 100 MiB maximum JPEG, 10 retained historical manifests plus all history from the last 30 days, and a 30-day grace period for separate maintenance cleanup. Automatic cleanup on publication does not wait for that grace period. Adjust these values in Lightroom’s **Storage and retention** section.

Release artifacts are unsigned and built for the Mac running `make package`. Build and test on Intel before distributing an Intel version. Windows credential storage and packaging are not implemented.

GitHub Actions runs the non-site Go, Lua, and schema checks on every push and pull request. Push an existing `v*` tag to build the Apple Silicon package, verify its checksum, and publish it with generated release notes; the Release workflow can also be dispatched manually for an existing tag.
