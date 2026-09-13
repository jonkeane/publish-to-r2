local Defaults = {
    profile = '',
    endpoint = '',
    bucket = '',
    publicBaseUrl = '',
    catalogPath = '',
    spoolLimitMiB = 5120,
    maxFileMiB = 100,
    historyKeep = 1,
    graceDays = 30,
    accessKeyId = '',
    secretAccessKey = '',
    galleryId = 'website',
    galleryTitle = 'Photographs',
    ownerName = '',
    publicLicense = '',
    fullSizeLongEdge = 4096,
    thumbnailShortEdge = 256,
    galleryShortEdge = 1024,
}

function Defaults.apply(settings)
    if settings == nil then
        return {}
    end
    for key, value in pairs(Defaults) do
        if settings[key] == nil then
            settings[key] = value
        end
    end
    return settings
end

return Defaults
