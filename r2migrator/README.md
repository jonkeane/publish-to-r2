# R2 stable-image-path migration

`migrate-public-base.go` moves existing public manifests from hash-addressed
JPEG paths to stable image coordinates: `large.jpg`, `gallery.jpg`, and
`thumbnail.jpg`. It makes R2 server-side copies from the existing JPEG objects
and then rewrites the manifest keys and URLs; it never downloads, renders, or
uploads image bytes. It reads the saved R2 Publisher profile and credentials from
`~/Library/Application Support/R2Publisher` (or `R2PUBLISHER_HOME`).

The default is a dry run. First inspect every current gallery manifest:

```sh
cd r2migrator
go run . --profile website --all
```

When the output is correct, repeat the command with `--apply`:

```sh
go run . --profile website --all --apply
```

To migrate one gallery, use its published gallery ID instead:

```sh
go run . --profile website --gallery GALLERY_ID --apply
```

`--base-url` is optional and defaults to the public base URL in the selected
profile. Supply it only when the manifest URLs should use a different HTTPS
domain.

For every changed manifest, the utility validates keys and hashes, copies each
legacy image to its stable coordinate, creates an immutable history revision,
and conditionally writes `current.json` with the ETag it read. If another
publisher updates that gallery, the manifest write fails rather than overwriting
it. Any copied JPEGs left by a failed conditional write are harmless; rerunning
uses the current manifest and completes the migration.

After applying it, run the normal `r2import` and Hugo build. The importer now
requires stable coordinates, so run this migration before its next import.
