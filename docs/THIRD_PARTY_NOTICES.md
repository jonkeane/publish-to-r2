# Third-party software

- AWS SDK for Go v2 and Smithy Go: Apache License 2.0. Versions are pinned in `uploader/go.mod` and `go.sum`. https://github.com/aws/aws-sdk-go-v2 and https://github.com/aws/smithy-go
- rxi/json.lua v0.1.2: MIT. Full copyright/license notice is preserved at the top of `R2Publisher.lrplugin/Json.lua`. https://github.com/rxi/json.lua

The build packages dependency license files under `docs/licenses/`. Adobe's SDK documentation was used to verify API names; SDK source/binaries are not redistributed. The mirrored API reference identifies itself as Adobe's 2007–2025 SDK documentation: https://lrc.mcor.dev/ . Actual behavior on the installed Lightroom 15.5 release still needs the manual pilot.

The photo-site checkout retains its own license and dependencies. This plugin does not reuse Jeffrey Friedl's plugin source.

## Cloudflare icon

`R2Publisher.lrplugin/cloudflare.svg` preserves the colored cloud paths from the `nav-logo-icon` on https://www.cloudflare.com/, retrieved September 10, 2026. The transparent 24 × 19 PNG is rendered from that vector with `rsvg-convert R2Publisher.lrplugin/cloudflare.svg -o R2Publisher.lrplugin/cloudflare.png`; the artwork keeps its original proportions and colors.

Cloudflare and the Cloudflare logo are trademarks and/or registered trademarks of Cloudflare, Inc. in the United States and other jurisdictions. The icon identifies the publishing destination; this plugin is not affiliated with or endorsed by Cloudflare. The logo is not covered by this project's software license. Cloudflare's logo-use terms are at https://www.cloudflare.com/trademark/ and require written permission for logo use other than the specified web badges.
