local LrExportSession = import 'LrExportSession'
local LrFileUtils = import 'LrFileUtils'
local LrPathUtils = import 'LrPathUtils'
local LrTasks = import 'LrTasks'
local LrProgressScope = import 'LrProgressScope'
local Renditions = {}
Renditions.fullSize = { key = 'fullSizeLongEdge', edge = 4096 }

-- Square CSS crops need pixels on the short edge, including hover enlargement.
Renditions.additional = {
    { name = 'thumbnail', key = 'thumbnailShortEdge', edge = 256 },
    { name = 'gallery', key = 'galleryShortEdge', edge = 1024 },
}

function Renditions.validateEdge(_, value)
    local edge = tonumber(value)
    if not edge or edge < 1 or edge >= math.huge or edge ~= math.floor(edge) then
        return false, value, 'Enter a positive whole number of pixels.'
    end
    return true, edge
end

function Renditions.constrain(settings, mode, edge)
    settings.LR_format = 'JPEG'
    settings.LR_export_colorSpace = 'sRGB'
    settings.LR_jpeg_quality = settings.LR_jpeg_quality or 0.85
    settings.LR_jpeg_useLimitSize = false
    settings.LR_size_doConstrain = true
    settings.LR_size_doNotEnlarge = true
    settings.LR_size_resizeType = mode
    -- SDK Guide: longEdge and shortEdge both read maxHeight.
    settings.LR_size_maxHeight = edge
    settings.LR_size_maxWidth = edge
    settings.LR_size_units = 'pixels'
end

local renderOptions = {
    'LR_jpeg_quality', 'LR_outputSharpeningOn', 'LR_outputSharpeningMedia',
    'LR_outputSharpeningLevel', 'LR_useWatermark', 'LR_watermarking_id',
    'LR_minimizeEmbeddedMetadata', 'LR_embeddedMetadataOption',
    'LR_metadata_keywordOptions', 'LR_removeLocationMetadata', 'LR_removeFaceMetadata',
    'LR_size_resolution', 'LR_size_resolutionUnits',
}

function Renditions.settings(original, edge)
    -- Copy rendering choices explicitly: never inherit a publish service or
    -- mutate Lightroom's observable property table in a secondary export.
    local settings = {
        LR_export_destinationType = 'tempFolder',
        -- Explicit sessions require a path even when Lightroom manages the
        -- temporary destination; the destination type alone is insufficient.
        LR_export_destinationPathPrefix = LrPathUtils.getStandardFilePath('temp'),
        LR_export_useSubfolder = false,
        LR_collisionHandling = 'rename',
        LR_renamingTokensOn = false,
        LR_extensionCase = 'lowercase',
        LR_reimportExportedPhoto = false,
    }
    for _, key in ipairs(renderOptions) do settings[key] = original[key] end
    Renditions.constrain(settings, 'shortEdge', edge)
    return settings
end

function Renditions.render(photo, settings, progress, stage, label, range)
    local paths = {}
    -- Keep only one additional rendition alive at once. This also avoids
    -- basename collisions and limits temporary disk usage for large catalogs.
    for i, spec in ipairs(Renditions.additional) do
        local value = settings[spec.key]
        if value == nil then value = spec.edge end -- Existing publish services.
        local valid, edge, message = Renditions.validateEdge(nil, value)
        assert(valid, spec.name .. ' short edge: ' .. tostring(message))
        assert(not progress:isCanceled(), 'Publishing cancelled during rendering')
        local caption = 'Rendering ' .. (label and label .. ' (' .. spec.name .. ')' or spec.name)
        -- Each iterator may reset its scope to zero or finish it at 100%.
        -- A fresh child confines those updates to this JPEG's share of the job.
        local scope = LrProgressScope {
            parent = progress,
            parentEndRange = range.start + (range.finish - range.start) * i / #Renditions.additional,
            caption = caption,
        }
        local success, problem = LrTasks.pcall(function()
            local session = LrExportSession {
                photosToExport = { photo }, exportSettings = Renditions.settings(settings, edge),
            }
            local count = 0
            for _, rendition in session:renditions {
                progressScope = scope, renderProgressPortion = 1, stopIfCanceled = true,
            } do
                assert(not progress:isCanceled() and not scope:isCanceled(), 'Publishing cancelled during rendering')
                local rendered, path = rendition:waitForRender()
                local ok, staged = LrTasks.pcall(function()
                    assert(rendered and not rendition.wasSkipped, tostring(path or 'Rendering was skipped'))
                    assert(not progress:isCanceled() and not scope:isCanceled(), 'Publishing cancelled during rendering')
                    return stage(spec.name, path)
                end)
                if rendered and path then LrFileUtils.delete(path) end
                if not ok then error(staged) end
                paths[spec.name] = staged
                count = count + 1
            end
            assert(count == 1, 'Additional JPEG did not render; gallery unchanged')
        end)
        if success then scope:setPortionComplete(1) end
        scope:done()
        if not success then error(problem) end
    end
    return paths
end

return Renditions
