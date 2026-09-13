local Bridge = require 'JobBridge'
local Identity = {}

-- Virtual copies may inherit custom metadata. Binding the UUID to the Lightroom
-- local ID repairs copied metadata without relying on names, paths or captions.
function Identity.get(photo)
    local owner = tostring(photo.localIdentifier)
    local id = photo:getPropertyForPlugin(_PLUGIN, 'photoUUID')
    if not id or photo:getPropertyForPlugin(_PLUGIN, 'identityOwner') ~= owner then
        local newId = Bridge.uuid()
        photo.catalog:withWriteAccessDo('Assign R2 photo identity', function()
            id = photo:getPropertyForPlugin(_PLUGIN, 'photoUUID')
            if not id or photo:getPropertyForPlugin(_PLUGIN, 'identityOwner') ~= owner then
                id = newId
                photo:setPropertyForPlugin(_PLUGIN, 'photoUUID', id)
                photo:setPropertyForPlugin(_PLUGIN, 'identityOwner', owner)
            end
        end, { timeout = 10 })
    end
    assert(id, 'Could not assign photo identity')
    return id
end

function Identity.gallery(profile, service, collection)
    return profile.serviceId .. '-' .. tostring(service.localIdentifier) .. '-' .. tostring(collection.localIdentifier)
end
return Identity
