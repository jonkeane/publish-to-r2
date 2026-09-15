-- Build Cloudflare dashboard URLs from the non-secret service profile. The
-- dashboard groups a photo's three public renditions under this folder.
local Dashboard = {}

local function escapePathSegment(value)
    return tostring(value):gsub('[^%w%-%._~]', function(character)
        return string.format('%%%02X', string.byte(character))
    end)
end

function Dashboard.photoFolderUrl(profile, photoId)
    if type(profile) ~= 'table' or type(profile.endpoint) ~= 'string'
        or type(profile.bucket) ~= 'string' or profile.bucket == ''
        or type(profile.namespace) ~= 'string' or profile.namespace == ''
        or photoId == nil or tostring(photoId) == '' then
        return nil
    end

    -- The configured S3 endpoint is the authoritative source of the account
    -- ID, and avoids asking users to enter the same value a second time.
    local accountId = profile.endpoint:match('^https://([a-fA-F0-9]+)%.r2%.cloudflarestorage%.com/?$')
    if not accountId then return nil end

    local prefix = 'photos%2F' .. escapePathSegment(profile.namespace) .. '%2F'
        .. escapePathSegment(photoId) .. '%2F'
    return 'https://dash.cloudflare.com/' .. accountId .. '/r2/default/buckets/'
        .. escapePathSegment(profile.bucket) .. '?prefix=' .. prefix
end

return Dashboard
