local LrView = import 'LrView'
local LrApplication = import 'LrApplication'
local LrTasks = import 'LrTasks'
local LrDialogs = import 'LrDialogs'
local Bridge = require 'JobBridge'
local Defaults = require 'Defaults'
local Renditions = require 'Renditions'
local Settings = {}
Settings.defaults = Defaults
Settings.fields = {
    -- Internal storage identity; retain the key for existing saved services.
    { key = 'profile', default = Defaults.profile },
    { key = 'endpoint', default = Defaults.endpoint },
    { key = 'bucket', default = Defaults.bucket },
    { key = 'publicBaseUrl', default = Defaults.publicBaseUrl },
    { key = 'catalogPath', default = Defaults.catalogPath },
    { key = 'spoolLimitMiB', default = Defaults.spoolLimitMiB },
    { key = 'maxFileMiB', default = Defaults.maxFileMiB },
    { key = 'historyKeep', default = Defaults.historyKeep },
    { key = 'graceDays', default = Defaults.graceDays },
    { key = 'accessKeyId', default = Defaults.accessKeyId },
    { key = 'secretAccessKey', default = Defaults.secretAccessKey },
    { key = 'galleryId', default = Defaults.galleryId },
    { key = 'galleryTitle', default = Defaults.galleryTitle },
    { key = 'ownerName', default = Defaults.ownerName },
    { key = 'publicLicense', default = Defaults.publicLicense },
    { key = Renditions.fullSize.key, default = Defaults.fullSizeLongEdge },
}
for _, spec in ipairs(Renditions.additional) do
    Settings.fields[#Settings.fields + 1] = { key = spec.key, default = Defaults[spec.key] }
end
function Settings.startDialog(props)
    Defaults.apply(props)
    LrTasks.startAsyncTask(function()
        Bridge.ensureProfile(props)
        Bridge.restoreSettings(props)
        if not props.catalogPath or props.catalogPath == '' then
            props.catalogPath = LrApplication.activeCatalog():getPath()
        end
    end)
end
function Settings.sections(f, props)
    local bind = LrView.bind
    local function field(label, key, password)
        local spec = { value = bind(key), width_in_chars = 35, immediate = true }
        return f:row { f:static_text { title = label, width = 150 },
            password and f:password_field(spec) or f:edit_field(spec) }
    end
    local function edgeField(label, key)
        return f:row { f:static_text { title = label, width = 150 },
            f:edit_field { value = bind(key), width_in_chars = 8, min = 1,
                precision = 0, validate = Renditions.validateEdge },
            f:static_text { title = 'pixels' } }
    end
    local connectionButtons = {
        f:push_button { title = 'Test Connection', action = function()
            LrTasks.startAsyncTask(function()
                local ok, result = LrTasks.pcall(Bridge.testConnection, props)
                LrDialogs.message('R2 connection', ok and result.message or tostring(result), ok and 'info' or 'critical')
            end)
        end }
    }
    if not props.LR_isExportForPublish then
        connectionButtons[#connectionButtons + 1] = f:push_button { title = 'Copy URL', action = function()
            LrTasks.startAsyncTask(function()
                local ok, result = LrTasks.pcall(function()
                    assert(props.publicBaseUrl and props.publicBaseUrl:match('^https://%S+$'), 'Enter the HTTPS public base URL')
                    return { publicBaseUrl = props.publicBaseUrl }
                end)
                if ok then
                    LrDialogs.presentModalDialog { title = 'Export gallery manifest URL',
                        contents = f:edit_field { value = result.publicBaseUrl:gsub('/$', '') .. '/galleries/' .. props.galleryId .. '/current.json', width_in_chars = 70 } }
                else LrDialogs.message('R2 settings', tostring(result), 'critical') end
            end)
        end }
    end
    local sections = { {
        title = 'Cloudflare R2', synopsis = bind('bucket'), bind_to_object = props,
        field('R2 S3 endpoint', 'endpoint'),
        field('Bucket', 'bucket'),
        field('Public base URL', 'publicBaseUrl'),
        field('Lightroom catalog', 'catalogPath'),
        field('R2 access key ID', 'accessKeyId'),
        field('R2 secret access key', 'secretAccessKey', true),
        f:static_text { title = 'Keys are saved in Lightroom settings and a private local file.', height_in_lines = 1 },
        f:row(connectionButtons),
    }, {
        title = 'Gallery and images', bind_to_object = props,
        field('Public owner name', 'ownerName'), field('Public license', 'publicLicense'),
        edgeField('Thumbnail short edge', 'thumbnailShortEdge'),
        edgeField('Gallery short edge', 'galleryShortEdge'),
        edgeField('Full size long edge', Renditions.fullSize.key),
    }, {
        title = 'Storage and retention', bind_to_object = props,
        field('Spool limit (MiB)', 'spoolLimitMiB'),
        field('Maximum JPEG (MiB)', 'maxFileMiB'),
        field('History versions', 'historyKeep'),
        field('Cleanup grace (days)', 'graceDays'),
    } }
    if not props.LR_isExportForPublish then
        table.insert(sections[2], 1, field('Export gallery ID', 'galleryId'))
        table.insert(sections[2], 2, field('Export gallery title', 'galleryTitle'))
    end
    return sections
end
return Settings
