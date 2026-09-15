-- Metadata panels cannot embed controls. Expose the same value through the
-- Library Plug-in Extras menu without assigning an identity as a side effect.
local LrApplication = import 'LrApplication'
local LrDialogs = import 'LrDialogs'
local LrTasks = import 'LrTasks'
local Bridge = require 'JobBridge'

LrTasks.startAsyncTask(function()
    local photos = LrApplication.activeCatalog():getTargetPhotos()
    if #photos ~= 1 then
        LrDialogs.message('Copy R2 photo identity', 'Select exactly one photo, then try again.', 'warning')
        return
    end

    local id = photos[1]:getPropertyForPlugin(_PLUGIN, 'photoUUID')
    if not id or id == '' then
        LrDialogs.message('Copy R2 photo identity',
            'This photo has no R2 identity yet. Publish it once, then try again.', 'warning')
        return
    end

    local ok, status = LrTasks.pcall(function()
        return LrTasks.execute('/usr/bin/printf %s ' .. Bridge.quote(id) .. ' | /usr/bin/pbcopy')
    end)
    if not (ok and status == 0) then
        LrDialogs.message('Copy R2 photo identity', 'Could not copy the identity. Try again.', 'critical')
    end
end)
