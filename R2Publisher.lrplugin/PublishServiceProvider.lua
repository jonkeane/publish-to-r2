local LrApplication = import 'LrApplication'
local LrHttp = import 'LrHttp'
local LrTasks = import 'LrTasks'
local LrView = import 'LrView'
local Provider = require 'ExportServiceProvider'
local Bridge = require 'JobBridge'
Provider.small_icon = 'cloudflare.png'
Provider.supportsIncrementalPublish = true
Provider.supportsCustomSortOrder = true
Provider.titleForPublishedCollection = 'R2 gallery'
Provider.titleForPublishedCollection_standalone = 'R2 gallery'

function Provider.deleteFirstOnPublish()
    -- Lightroom defaults to deleting last. An upload ownership conflict must
    -- not prevent removal of a legacy photo shared by multiple galleries.
    return true
end

function Provider.getCollectionBehaviorInfo()
    return { defaultCollectionName = 'Photographs', canAddCollection = true,
        maxCollectionSetDepth = 0 }
end
function Provider.metadataThatTriggersRepublish()
    return { default = true, gps = false, gpsAltitude = false, rating = false, label = false }
end
function Provider.viewForCollectionSettings(f, settings, info)
    -- Keep display state transient; the saved remote ID is owned by publishing.
    local props = info.pluginContext
    props.galleryId = ''
    props.canCopyGalleryId = false
    props.galleryIdHelp = 'Publish this collection first to assign its gallery ID.'
    if info.publishedCollection then
        props.galleryIdHelp = 'Loading gallery ID...'
        LrTasks.startAsyncTask(function()
            local ok, id = LrTasks.pcall(function()
                return info.publishedCollection:getRemoteId()
            end)
            if not ok then
                props.galleryIdHelp = 'Could not read the gallery ID. Close this dialog and try again.'
            elseif id and tostring(id) ~= '' then
                props.galleryId = tostring(id)
                props.canCopyGalleryId = true
                props.galleryIdHelp = ''
            else
                props.galleryIdHelp = 'Publish this collection first to assign its gallery ID.'
            end
        end)
    end
    return f:group_box {
        title = 'Cloudflare R2', bind_to_object = props,
        f:row {
            f:static_text { title = 'Gallery ID', width = 100 },
            f:static_text { title = LrView.bind('galleryId'), selectable = true, width_in_chars = 55 },
            f:push_button {
                title = 'Copy Gallery ID', enabled = LrView.bind('canCopyGalleryId'),
                action = function()
                    if not props.canCopyGalleryId then return end
                    local id = props.galleryId
                    props.canCopyGalleryId = false
                    LrTasks.startAsyncTask(function()
                        local ok, status = LrTasks.pcall(function()
                            -- This plugin supports macOS. Quote the ID as data and
                            -- use a fixed printf format to copy without a newline.
                            return LrTasks.execute('/usr/bin/printf %s ' .. Bridge.quote(id) .. ' | /usr/bin/pbcopy')
                        end)
                        props.galleryIdHelp = ok and status == 0 and 'Copied.' or 'Could not copy the gallery ID. Try again.'
                        props.canCopyGalleryId = true
                    end)
                end,
            },
        },
        f:static_text { title = LrView.bind('galleryIdHelp'), width_in_chars = 65, height_in_lines = 2 },
    }
end
local function mutate(settings, gallery, title, removals, order, removeAll)
    if not gallery then return end -- An unpublished collection has no remote state.
    return Bridge.withPublisher(nil, gallery, function()
        local prepared = Bridge.prepare(settings, gallery, title)
        prepared.job.removals = removals or {}
        if removeAll then
            for _, photo in ipairs(prepared.entries) do prepared.job.removals[#prepared.job.removals + 1] = photo.id end
        end
        if order then prepared.job.order = order end
        return Bridge.run(prepared)
    end)
end
function Provider.deletePhotosFromPublishedCollection(settings, photoIds, deletedCallback, localCollectionId)
    local collection = LrApplication.activeCatalog():getPublishedCollectionByLocalIdentifier(localCollectionId)
    assert(collection, 'Published collection could not be found')
    local info = collection:getCollectionInfoSummary()
    assert(info.remoteId, 'Published collection has no remote identity')
    mutate(settings, info.remoteId, info.name, photoIds)
    for _, id in ipairs(photoIds) do deletedCallback(id) end
end
function Provider.renamePublishedCollection(settings, info)
    mutate(settings, info.remoteId, info.name)
end
function Provider.deletePublishedCollection(settings, info)
    mutate(settings, info.remoteId, info.name, nil, nil, true)
end
function Provider.imposeSortOrderOnPublishedCollection(settings, info, sequence)
    mutate(settings, info.remoteId or info.remoteCollectionId, info.name, nil, sequence)
end
function Provider.goToPublishedCollection(settings, info)
    local url = info.publishedCollectionInfo.remoteUrl
    if url then LrHttp.openUrlInBrowser(url) end
end
function Provider.goToPublishedPhoto(settings, info)
    local url = info.publishedPhoto:getRemoteUrl()
    if url then LrHttp.openUrlInBrowser(url) end
end
return Provider
