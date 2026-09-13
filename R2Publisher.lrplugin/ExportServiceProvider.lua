local LrTasks = import 'LrTasks'
local LrDialogs = import 'LrDialogs'
local LrProgressScope = import 'LrProgressScope'
local Bridge = require 'JobBridge'
local Identity = require 'PhotoIdentity'
local Metadata = require 'PublicMetadata'
local Settings = require 'Settings'
local Renditions = require 'Renditions'
local Provider = {
    allowFileFormats = { 'JPEG' },
    allowColorSpaces = { 'sRGB' },
    canExportVideo = false,
    hideSections = { 'exportLocation', 'fileNaming', 'imageSettings' },
    exportPresetFields = Settings.fields,
    sectionsForTopOfDialog = Settings.sections,
    startDialog = Settings.startDialog,
}

function Provider.updateExportSettings(settings)
    local value = settings[Renditions.fullSize.key]
    if value == nil then value = Renditions.fullSize.edge end -- Existing publish services.
    local valid, edge, message = Renditions.validateEdge(nil, value)
    assert(valid, 'Full size long edge: ' .. tostring(message))
    Renditions.constrain(settings, 'longEdge', edge)
end

function Provider.processRenderedPhotos(functionContext, exportContext)
    local settings = exportContext.propertyTable
    -- Retain Lightroom's publish UI, but give every renderer an explicit child
    -- scope. The context-managed primary pipeline can briefly fill the shared
    -- bar even with renderPortion set, so do not start it via the context.
    local primaryPortion = 0.4 / (1 + #Renditions.additional)
    local progress = exportContext:configureProgress {
        title = 'Publishing gallery to R2', renderPortion = primaryPortion,
    }
    progress:setPortionComplete(0)
    local renditions = {}
    local warnings = {}
    local ok, problem = LrTasks.pcall(function()
        local gallery, title = settings.galleryId, settings.galleryTitle
        local queueGallery = gallery
        if exportContext.publishedCollection then
            gallery = exportContext.publishedCollectionInfo.remoteId
            title = exportContext.publishedCollectionInfo.name
            queueGallery = gallery or ('collection:' .. tostring(exportContext.publishService.localIdentifier) ..
                ':' .. tostring(exportContext.publishedCollection.localIdentifier))
        end
        return Bridge.withPublisher(progress, queueGallery, function()
            local profile = Bridge.profile(settings)
            if exportContext.publishedCollection then
                gallery = gallery or Identity.gallery(profile, exportContext.publishService, exportContext.publishedCollection)
            end
            Bridge.identifyGallery(queueGallery, gallery)
            local prepared = Bridge.prepare(settings, gallery, title)
            local failed = false
            local total = exportContext.exportSession:countRenditions()
            local jpegTotal = total * (1 + #Renditions.additional)
            local function stage(id, path, name)
                assert(not progress:isCanceled(), 'Publishing cancelled during rendering')
                return Bridge.stage(settings, prepared, id, path, name)
            end
            local rendering = LrProgressScope { parent = progress, parentEndRange = primaryPortion }
            local renderOK, renderProblem = LrTasks.pcall(function()
                -- The session iterator starts rendering itself, as for secondary
                -- sessions. Its 0..1 updates now affect only the primary allocation.
                for _, rendition in exportContext.exportSession:renditions {
                    progressScope = rendering, renderProgressPortion = 1, stopIfCanceled = true,
                } do
                    assert(not progress:isCanceled() and not rendering:isCanceled(), 'Publishing cancelled during rendering')
                    renditions[#renditions + 1] = rendition
                    rendering:setCaption('Rendering photo ' .. #renditions .. ' of ' .. total .. ' (full size)')
                    local rendered, path = rendition:waitForRender()
                    assert(not progress:isCanceled() and not rendering:isCanceled(), 'Publishing cancelled during rendering')
                    if rendered and not rendition.wasSkipped then
                        local id = Identity.get(rendition.photo)
                        local staged = stage(id, path)
                        prepared.job.photos[#prepared.job.photos + 1] = {
                            id = id, path = staged, metadata = Metadata.read(rendition.photo, settings),
                        }
                    else
                        failed = true
                        rendition:uploadFailed(tostring(path or 'Rendering was skipped'))
                    end
                end
                assert(not progress:isCanceled() and not rendering:isCanceled(), 'Publishing cancelled; no gallery changes committed')
                assert(not failed, 'One or more JPEGs failed to render; gallery unchanged')
            end)
            if renderOK then rendering:setPortionComplete(1) end
            rendering:done()
            if not renderOK then error(renderProblem) end
            for i, rendition in ipairs(renditions) do
                local photo = prepared.job.photos[i]
                local firstJPEG = total + (i - 1) * #Renditions.additional
                photo.renditions = Renditions.render(rendition.photo, settings, progress, function(name, renderedPath)
                    return stage(photo.id, renderedPath, name)
                end, 'photo ' .. i .. ' of ' .. total, {
                    start = 0.4 * firstJPEG / jpegTotal,
                    finish = 0.4 * (firstJPEG + #Renditions.additional) / jpegTotal,
                })
            end
            assert(not progress:isCanceled(), 'Publishing cancelled; no gallery changes committed')
            local result = Bridge.run(prepared, progress)
            progress:setCaption('Updating Lightroom')
            if exportContext.publishedCollection then
                exportContext.exportSession:recordRemoteCollectionId(gallery)
                exportContext.exportSession:recordRemoteCollectionUrl(result.manifestUrl)
                for i, rendition in ipairs(renditions) do
                    local uploaded = result.photos[i]
                    assert(uploaded and uploaded.status == 'committed', 'Missing committed photo result')
                    rendition:recordPublishedPhotoId(uploaded.id)
                    rendition:recordPublishedPhotoUrl(uploaded.url)
                end
            end
            progress:setPortionComplete(1)
            warnings = result.warnings or {}
        end)
    end)
    progress:done()
    if not ok then
        for _, rendition in ipairs(renditions) do rendition:uploadFailed(tostring(problem)) end
        error(problem)
    end
    if #warnings > 0 then
        LrDialogs.message('Export finished — gallery cover warning',
            'Upload completed successfully.\n\n' .. table.concat(warnings, '\n\n'), 'warning')
    end
end
return Provider
