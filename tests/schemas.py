"""Check protocol examples using the schema keywords used by this project."""
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

def validate(value, schema):
    if "const" in schema:
        assert value == schema["const"]
    if "enum" in schema:
        assert value in schema["enum"]
    kind = schema.get("type")
    if kind == "object":
        assert isinstance(value, dict)
        assert set(schema.get("required", [])) <= value.keys()
        assert value.keys() <= schema["properties"].keys()
        for key, item in value.items():
            validate(item, schema["properties"][key])
    elif kind == "array":
        assert isinstance(value, list)
        for item in value:
            validate(item, schema["items"])
    elif kind == "string":
        assert isinstance(value, str)
        if "pattern" in schema:
            assert re.search(schema["pattern"], value)
    elif kind == "integer":
        assert isinstance(value, int) and not isinstance(value, bool)
        assert value >= schema.get("minimum", value)

job = dict(version=1, jobId="job", profile="website", catalogPath="/Catalog.lrcat",
           namespace="catalog", serviceId="service", galleryId="gallery", expectedRevision="",
           title="日本", photos=[dict(id="photo", path="/spool/job/photo.jpg", metadata={"tags": ["Film"]})], removals=[])
manifest = dict(schemaVersion=1, revision="revision", parentRevision="", galleryId="gallery", namespace="catalog",
                serviceId="service", title="日本", updatedAt="2026-09-09T12:00:00Z", entries=[])
result = dict(version=1, jobId="job", status="committed", revision="revision", photos=[],
              warnings=['Gallery has 0 photos with the tag gallery-cover.'])
for name, example in [("job", job), ("manifest", manifest), ("result", result)]:
    schema = json.loads((ROOT / "schemas" / f"{name}-v1.json").read_text())
    validate(example, schema)
    invalid = {**example, "secretAccessKey": "must not appear"}
    try:
        validate(invalid, schema)
    except AssertionError:
        pass
    else:
        raise AssertionError("unknown field accepted")
# The optional rendition set is complete and contains public image data only.
job["photos"][0]["renditions"] = {name: f"/spool/job/photo.{name}.jpg" for name in ["thumbnail", "gallery"]}
image = dict(key="photos/catalog/photo/"+"a"*64+".jpg", source="https://images.example.com/photos/catalog/photo/"+"a"*64+".jpg", width=384, height=256, bytes=200, sha256="a"*64)
manifest["entries"] = [dict(image, id="photo", originalFormat="jpg", dateUpload="1", lastUpdate="1", renditions={"thumbnail":image.copy(), "gallery":image.copy()})]
manifest["entries"][0]["exif"] = {"emulsion": "Kodak Portra 400", "make": "Camera maker", "iso": "800"}
job["photos"][0]["metadata"]["exif"] = {"emulsion": "Kodak Portra 400", "make": "Camera maker"}
for name, example in [("job",job),("manifest",manifest)]:
    schema=json.loads((ROOT / "schemas" / f"{name}-v1.json").read_text())
    validate(example,schema)
    records=example["photos" if name=="job" else "entries"]
    del records[0]["renditions"]["thumbnail"]
    try:
        validate(example,schema)
    except AssertionError:
        pass
    else:
        raise AssertionError("incomplete rendition set accepted")
print("Job, manifest and result schema examples, including renditions, passed")
