`film.jpg` is a synthetic 2×2 black JPEG generated with Go's `image/jpeg`,
then tagged with ExifTool 13.59 using:

```sh
exiftool -config docs/analog-film-exiftool.config -overwrite_original \
  '-XMP-AnalogExif:Film=Kodak Portra 400' \
  uploader/internal/photometadata/testdata/film.jpg
```

It exercises actual ExifTool XMP output without a runtime ExifTool dependency.


`technical.jpg` starts as a copy of `film.jpg` and adds standard EXIF using
ExifTool 13.59 (the GPS and Make values are deliberate allowlist tests):

```sh
exiftool -overwrite_original \
  '-EXIF:Model=Film Camera' '-EXIF:LensModel=50mm f/1.4' \
  '-EXIF:FocalLength=50' '-EXIF:FNumber=5.6' '-EXIF:ExposureTime=1/125' \
  '-EXIF:ISO=800' '-EXIF:DateTimeOriginal=2025:07:06 14:03:02' \
  '-EXIF:SubSecTimeOriginal=125' '-EXIF:OffsetTimeOriginal=-05:00' \
  '-EXIF:Flash=Fired' '-EXIF:Make=Not film stock' \
  '-EXIF:GPSLatitude=41.88' '-EXIF:GPSLongitude=-87.63' \
  uploader/internal/photometadata/testdata/technical.jpg
```
