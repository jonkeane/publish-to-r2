local Metadata = {}
local function value(photo, name)
    local v = photo:getFormattedMetadata(name)
    if v == nil then return '' end
    return tostring(v)
end
function Metadata.read(photo, settings)
    local tags = {}
    for tag in value(photo, 'keywordTagsForExport'):gmatch('[^,]+') do
        local trimmedTag = tag:match('^%s*(.-)%s*$')
        if trimmedTag ~= '' then tags[#tags + 1] = trimmedTag end
    end
    local filename = value(photo, 'preservedFileName')
    if filename == '' then filename = value(photo, 'fileName') end
    filename = filename:match('[^/\\]+$') or ''
    local title = value(photo, 'title')
    if title == '' then title = value(photo, 'fileName') end
    local caption = value(photo, 'caption')
    return {
        title = title, caption = caption, alt = caption ~= '' and caption or title,
        copyright = value(photo, 'copyright'), tags = tags,
        ownerName = settings.ownerName ~= '' and settings.ownerName or value(photo, 'creator'),
        license = settings.publicLicense or '',
        -- The helper reads technical EXIF and film XMP from the finished JPEG.
        -- Only the catalog can reliably supply the original preserved filename.
        exif = { preservedfilename = filename },
    }
end
return Metadata
