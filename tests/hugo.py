"""Compile real site adapters/layouts against small deterministic local fixtures."""
import json
import shutil
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
(ROOT / ".cache").mkdir(exist_ok=True)
with tempfile.TemporaryDirectory(prefix="hugo-test-", dir=ROOT / ".cache") as temporary:
    work = Path(temporary)
    def write(path, value):
        target = work / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(value)
    write("hugo.toml", '''baseURL = "https://site.example.com/"
title = "Photo test"
[params]
emulsions = ["Kodak Portra"]
[params.posts]
pagesize = 2
[taxonomies]
tag = "tags"
camera = "cameras"
lens = "lenses"
emulsion = "emulsions"
''')
    paths = ["content/gallery/_content.gotmpl", "layouts/gallery/single.html", "layouts/gallery/list.html",
             "layouts/partials/opengraph.html", "layouts/partials/twitter_cards.html"]
    paths += [str(p.relative_to(ROOT / "site")) for p in (ROOT / "site/layouts/partials/photo").glob("*.html")]
    paths += [str(p.relative_to(ROOT / "site")) for p in (ROOT / "site/layouts/partials/gallerys").glob("*.html")]
    for path in paths:
        write(path, (ROOT / "site" / path).read_text())
    for name in ["header", "nav", "footer/index", "copyright", "scripts/index"]:
        write(f"layouts/partials/{name}.html", "")
    write("layouts/partials/htmlhead.html", '''<!doctype html><html><head><title>{{ .Title }}</title>
{{ partial "opengraph.html" . }}{{ partial "twitter_cards.html" . }}</head>''')
    write("layouts/partials/posts/pagination.html", '{{ range .paginator.Pagers }}<a href="{{ .URL }}">{{ .PageNumber }}</a>{{ end }}')
    write("layouts/_default/list.html", '{{ range .Pages }}<a href="{{ .RelPermalink }}">{{ .Title }}</a>{{ end }}')
    write("layouts/index.html", '''{{ $galleries := (site.GetPage "gallery").Sections }}
{{ $data := dict "post" (dict "linktext" "View gallery") }}
<div id="cards">{{ partial "gallerys/list" (dict "posts" (dict "Pages" $galleries) "siteData" $data) }}</div>
<div id="featured">{{ partial "gallerys/featured" (dict "firstpost" $galleries "siteData" $data) }}</div>
<div id="frontpage">{{ partial "gallerys/frontpage" (dict "firstpost" $galleries "siteData" $data) }}</div>''')
    write("content/gallery/_index.md", "---\ntitle: Galleries\n---\n")
    write("content/gallery/one/_index.md", '---\ntitle: One\nr2_gallery_id: "gallery"\nimage: fallback.jpg\n---\n')
    (work / "assets").mkdir()
    shutil.copyfile(ROOT / "site/static/photo-jonkeane.jpg", work / "assets/fallback.jpg")
    write("content/gallery/legacy/_index.md", '---\ntitle: Legacy\n---\n')
    legacy = {"photoset": {"id": "flickr", "photo": [{"id": "old-photo", "title": "Old", "tags": ["legacy"],
              "datetaken": "2025-01-01 12:00:00", "description": {"_content": "Flickr"}, "exif": {"model": "Camera", "lens": "Lens"},
              "url_z": "https://live.staticflickr.com/old.jpg", "url_s": "https://live.staticflickr.com/old.jpg"}]}}
    write("data/flickr/photosets/one.json", json.dumps(legacy))
    write("data/flickr/photosets/legacy.json", json.dumps(legacy))
    entries = []
    for i in range(4):
        key = f"photos/catalog/photo-{i}/{'a' * 64}.jpg"
        entries.append(dict(id=f"photo-{i}", title=f"Photo {i}", caption="<script>bad()</script> & 日本", tags=["r2-test"],
            dateTaken=f"2025-07-0{i+1}T12:00:00Z", source="https://images.example.com/"+key, width=4096, height=2731,
            renditions=dict(thumbnail=dict(source=f"https://images.example.com/thumb-{i}.jpg",width=384,height=256),
                            gallery=dict(source=f"https://images.example.com/grid-{i}.jpg",width=1536,height=1024)),
            exif=dict(model="Camera", lens="Lens", iso="800", preservedfilename="original.tif")))
    # Explicit film stock wins without relabeling its speed to the rated ISO.
    entries[0]["exif"]["emulsion"] = "Kodak Portra 400"
    entries[1]["exif"]["emulsion"] = "Custom Film 日本"
    entries[1]["exif"]["model"] = "Kodak Portra Camera"
    # Legacy camera-prefix inference remains available when Film is absent.
    entries[2]["exif"]["model"] = "Kodak Portra Camera"
    # Mix current and legacy single-image entries in one manifest.
    entries[-1].pop("renditions")
    manifest = dict(schemaVersion=1, galleryId="gallery", entries=entries)
    write("data/r2/galleries/one.json", json.dumps(manifest))
    def build():
        shutil.rmtree(work / "public", ignore_errors=True)
        subprocess.run(["hugo", "--source", str(work), "--destination", str(work / "public"), "--cleanDestinationDir"], check=True, capture_output=True)
    try:
        build()
        public = work / "public"
        assert (public / "gallery/one/photo-0/index.html").exists()
        assert not (public / "gallery/one/old-photo/index.html").exists(), "R2 did not replace Flickr gallery"
        assert (public / "gallery/legacy/old-photo/index.html").exists(), "Flickr gallery disappeared"
        assert (public / "gallery/one/page/2/index.html").exists(), "Hugo pagination missing"
        html = (public / "gallery/one/photo-0/index.html").read_text()
        assert "https://images.example.com/photos/catalog/photo-0/" in html
        assert "&lt;script&gt;bad()&lt;/script&gt;" in html and "<script>bad()</script>" not in html
        assert 'src="https://images.example.com/thumb-0.jpg"' in html
        assert 'width="4096" height="2731"' in html
        assert "max-width: min(100%, 2048px)" in html and "max-height: min(100%, 1365.5px)" in html
        assert "srcset=" not in html, "R2 strip offered a gallery image as a fake 2x thumbnail"
        grid = (public / "gallery/one/index.html").read_text()
        assert 'src="https://images.example.com/grid-0.jpg"' in grid
        assert "fallback_" in grid, "existing social cover was lost"
        assert "fallback_" in (public / "index.html").read_text(), "existing gallery cover was lost"
        fallback = (public / "gallery/one/photo-3/index.html").read_text()
        assert 'src="https://images.example.com/photos/catalog/photo-3/' in fallback
        assert "photostrip" in html and "/gallery/one/photo-1/" in html
        assert "/gallery/one/photo-0/" in (public / "tags/r2-test/index.html").read_text()
        assert (public / "cameras/camera/index.html").exists() and (public / "lenses/lens/index.html").exists()
        assert (public / "emulsions/kodak-portra-400/index.html").exists()
        assert (public / "emulsions/kodak-portra-800/index.html").exists(), "legacy emulsion inference lost"
        assert (public / "cameras/kodak-portra-camera/index.html").exists(), "explicit film rewrote camera name"
        film_html = (public / "gallery/one/photo-0/index.html").read_text()
        assert "Kodak Portra 400" in film_html
        assert not (public / "emulsions/kodak-portra-400-800/index.html").exists(), "rated ISO changed the stock taxonomy"
        assert "Custom Film 日本" in (public / "gallery/one/photo-1/index.html").read_text()

        # A dedicated crop overrides the old asset without becoming a photo page.
        cover = dict(entries[0], id="cover-only", title="Cover crop", alt='Crop & "sky"',
                     tags=[" Gallery-Cover ", "cover-exclusive"], source="https://images.example.com/cover-large.jpg",
                     renditions=dict(gallery=dict(source="https://images.example.com/cover-grid.jpg", width=1536, height=1024),
                                     thumbnail=dict(source="https://images.example.com/cover-thumb.jpg", width=384, height=256)))
        manifest["entries"] = entries + [cover]
        write("content/gallery/one/_index.md", '---\ntitle: One\nr2_gallery_id: "gallery"\nimage: unused-old-cover.jpg\n---\n')
        write("data/r2/galleries/one.json", json.dumps(manifest)); build()
        home = (public / "index.html").read_text()
        assert home.count('src="https://images.example.com/cover-grid.jpg"') == 2
        assert 'data-srcset="https://images.example.com/cover-grid.jpg"' in home
        assert '<picture>\n            <img data-srcset="https://images.example.com/cover-grid.jpg"' in home, "homepage lazy loader needs a picture child"
        assert 'alt="Crop &amp; &#34;sky&#34;"' in home
        assert not (public / "gallery/one/cover-crop/index.html").exists()
        assert not (public / "tags/gallery-cover/index.html").exists()
        assert not (public / "tags/cover-exclusive/index.html").exists()
        assert not (public / "gallery/one/page/3/index.html").exists(), "cover affected pagination"
        grid = (public / "gallery/one/index.html").read_text()
        assert '<meta property="og:image" content="https://images.example.com/cover-grid.jpg"' in grid
        assert '<meta name="twitter:image" content="https://images.example.com/cover-grid.jpg"' in grid
        assert 'src="https://images.example.com/cover-' not in grid
        for i in range(4):
            photo_html = (public / f"gallery/one/photo-{i}/index.html").read_text()
            assert "cover-crop" not in photo_html and "cover-grid.jpg" not in photo_html

        # Old single-image manifests can supply covers too.
        cover.pop("renditions")
        write("data/r2/galleries/one.json", json.dumps(manifest)); build()
        assert 'src="https://images.example.com/cover-large.jpg"' in (public / "index.html").read_text()

        # Ambiguous covers must fail rather than silently choose one.
        manifest["entries"] = entries + [cover, dict(cover, id="second-cover")]
        write("data/r2/galleries/one.json", json.dumps(manifest))
        try:
            build()
            raise AssertionError("multiple gallery covers were accepted")
        except subprocess.CalledProcessError as error:
            assert "multiple gallery-cover photos" in (error.stdout + error.stderr).decode()

        # A collection containing only its cover still builds without photo pages.
        manifest["entries"] = [cover]
        write("data/r2/galleries/one.json", json.dumps(manifest)); build()
        assert 'src="https://images.example.com/cover-large.jpg"' in (public / "index.html").read_text()
        assert not (public / "gallery/one/cover-crop/index.html").exists()
        assert not (public / "gallery/one/photo-0/index.html").exists()

        # Removing the keyword restores a normal gallery photo.
        cover["tags"] = ["r2-test"]
        manifest["entries"] = entries + [cover]
        write("content/gallery/one/_index.md", '---\ntitle: One\nr2_gallery_id: "gallery"\n---\n')
        write("data/r2/galleries/one.json", json.dumps(manifest)); build()
        assert (public / "gallery/one/cover-crop/index.html").exists()
        assert (public / "gallery/one/page/3/index.html").exists()

        # Flickr uses the same marker; R2 still takes precedence for migrated galleries.
        legacy["photoset"]["photo"].append(dict(legacy["photoset"]["photo"][0], id="legacy-cover", tags=["gallery-cover"],
                                               url_z="https://live.staticflickr.com/cover.jpg"))
        write("data/flickr/photosets/legacy.json", json.dumps(legacy))
        write("data/flickr/photosets/one.json", json.dumps(legacy)); build()
        assert not (public / "gallery/legacy/legacy-cover/index.html").exists()
        assert '<meta property="og:image" content="https://live.staticflickr.com/cover.jpg"' in (public / "gallery/legacy/index.html").read_text()
        assert "staticflickr.com/cover.jpg" not in (public / "gallery/one/index.html").read_text()
        manifest["entries"] = []
        write("data/r2/galleries/one.json", json.dumps(manifest)); build()
        assert not (public / "gallery/one/photo-0/index.html").exists(), "removed photo page survived a clean build"
        assert not (public / "gallery/one/old-photo/index.html").exists(), "empty R2 gallery fell back to Flickr"
        assert "staticflickr.com/cover.jpg" not in (public / "gallery/one/index.html").read_text()
    except subprocess.CalledProcessError as error:
        print(error.stdout.decode(), error.stderr.decode())
        raise
print("Hugo pages, pagination, navigation, taxonomies, covers, caption escaping and removal checks passed")
