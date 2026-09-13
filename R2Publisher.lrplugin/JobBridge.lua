local LrTasks = import 'LrTasks'
local LrFileUtils = import 'LrFileUtils'
local LrPathUtils = import 'LrPathUtils'
local LrApplication = import 'LrApplication'
local Json = require 'Json'
local Defaults = require 'Defaults'
local Bridge = {}
local publishers = {}
local galleryIdentities = {}

function Bridge.defaults()
    local values = {}
    for key, value in pairs(Defaults) do values[key] = value end
    return values
end

-- A first publish starts with a local collection key. Once its remote ID is
-- known, queued exports and later collection callbacks must share that identity.
function Bridge.identifyGallery(localKey, gallery)
    if localKey ~= gallery then galleryIdentities[localKey] = gallery end
end

-- require() is cached per plugin, so export and collection callbacks share
-- this scheduler. Keep each gallery FIFO while allowing two galleries at once.
local maxPublishers = 2
local function canStart(ticket)
    for _, queued in ipairs(publishers) do
        if queued == ticket then break end
        if (galleryIdentities[queued.gallery] or queued.gallery) ==
            (galleryIdentities[ticket.gallery] or ticket.gallery) then return false end
    end
    -- Later tickets may already be active for a different gallery.
    local active = 0
    for _, queued in ipairs(publishers) do
        if queued.active then active = active + 1 end
    end
    return active < maxPublishers
end
function Bridge.withPublisher(progress, gallery, action)
    assert(gallery and gallery ~= '', 'Missing gallery identity')
    local ticket = { gallery = gallery }
    publishers[#publishers + 1] = ticket
    local ok, result = LrTasks.pcall(function()
        if progress and not canStart(ticket) then
            progress:setCaption('Waiting for a publishing slot or an earlier update to this gallery')
        end
        while not canStart(ticket) do
            assert(not progress or not progress:isCanceled(), 'Publishing cancelled while waiting')
            LrTasks.sleep(0.1)
        end
        assert(not progress or not progress:isCanceled(), 'Publishing cancelled before starting')
        ticket.active = true
        if progress then progress:setCaption('Preparing gallery') end
        return action()
    end)
    for i, queued in ipairs(publishers) do
        if queued == ticket then table.remove(publishers, i); break end
    end
    if not ok then error(result) end
    return result
end

function Bridge.quote(value)
    value = tostring(value)
    assert(not value:find('%z'), 'NUL is not allowed in process arguments')
    return "'" .. value:gsub("'", "'\\''") .. "'"
end
function Bridge.write(path, value)
    assert(not WIN_ENV, 'R2 Publisher currently supports macOS only')
    local data = Json.encode(value)
    local temporary = path .. '.tmp'
    local f = assert(io.open(temporary, 'wb'), 'Cannot write job file')
    local written, writeError = f:write(data)
    local closed, closeError = f:close()
    assert(written, writeError or 'Cannot write job file')
    assert(closed, closeError or 'Cannot close job file')
    -- Lightroom omits os.rename, and LrFileUtils.move cannot replace the job
    -- created by init. A same-directory macOS move preserves atomic replacement.
    local status = LrTasks.execute('/bin/mv -f ' .. Bridge.quote(temporary) .. ' ' .. Bridge.quote(path))
    assert(status == 0, 'Cannot save job file; temporary job retained at ' .. temporary)
end
function Bridge.read(path)
    if not LrFileUtils.exists(path) then return nil end
    local data = LrFileUtils.readFile(path)
    if data == '' then return nil end -- os.tmpname may reserve an empty file.
    return Json.decode(data)
end
function Bridge.uuid()
    assert(not WIN_ENV, 'R2 Publisher currently supports macOS only')
    -- LrUUID is undocumented. os.tmpname is part of Lightroom's supported Lua
    -- subset; run the system UUID generator through the documented task API.
    local response = os.tmpname()
    local ok, result = LrTasks.pcall(function()
        assert(LrTasks.execute('/usr/bin/uuidgen > ' .. Bridge.quote(response)) == 0, 'Cannot generate photo identity')
        local id = LrFileUtils.readFile(response):match('^%s*(%x%x%x%x%x%x%x%x%-%x%x%x%x%-%x%x%x%x%-%x%x%x%x%-%x%x%x%x%x%x%x%x%x%x%x%x)%s*$')
        return assert(id, 'Invalid photo identity from uuidgen')
    end)
    LrFileUtils.delete(response)
    if not ok then error(result) end
    return result
end
local function helperCommand(executable, arguments, responsePath)
    local command = Bridge.quote(executable)
    for _, value in ipairs(arguments) do command = command .. ' ' .. Bridge.quote(value) end
    command = command .. ' --out ' .. Bridge.quote(responsePath) .. ' >/dev/null 2>/dev/null'
    return command
end

local function helperInputPath(responsePath)
    return responsePath .. '.credentials/input.json'
end

function Bridge.runHelper(arguments, input)
    assert(not WIN_ENV, 'R2 Publisher currently supports macOS only')
    local executable = LrPathUtils.child(_PLUGIN.path, 'bin/r2publisher')
    assert(LrFileUtils.exists(executable), 'Build/install the bundled r2publisher helper first')
    local responsePath = os.tmpname()
    local command = helperCommand(executable, arguments, responsePath)
    local inputDirectory
    local ok, status = LrTasks.pcall(function()
        if input then
            local directory = responsePath .. '.credentials'
            assert(LrTasks.execute('/bin/mkdir -m 700 ' .. Bridge.quote(directory)) == 0,
                'Cannot create private credential directory')
            inputDirectory = directory
            local inputPath = helperInputPath(responsePath)
            local f = assert(io.open(inputPath, 'wb'), 'Cannot write credentials')
            local written, writeError = f:write(Json.encode(input))
            local closed, closeError = f:close()
            assert(written, writeError or 'Cannot write credentials')
            assert(closed, closeError or 'Cannot close credential file')
            command = command .. ' < ' .. Bridge.quote(inputPath)
        end
        return LrTasks.execute(command)
    end)
    if inputDirectory then
        LrFileUtils.delete(helperInputPath(responsePath))
        LrFileUtils.delete(inputDirectory)
    end
    local readOK, result = LrTasks.pcall(Bridge.read, responsePath)
    LrFileUtils.delete(responsePath)
    if not ok then error(status) end
    if status ~= 0 then error(readOK and result and result.message or 'R2 helper failed; try Test Connection in Lightroom') end
    if not readOK then error(result) end
    assert(result, 'R2 helper returned no result')
    return result
end

function Bridge.call(arguments, input)
    return Bridge.runHelper(arguments, input)
end
-- Existing names must continue to address the same credentials and identities.
-- New settings get an opaque ID that Lightroom saves with the service/preset.
function Bridge.ensureProfile(settings)
    if not settings.profile or settings.profile == '' then
        local id = Bridge.uuid()
        -- UUID generation yields; another task may have initialized it meanwhile.
        if not settings.profile or settings.profile == '' then settings.profile = id end
    end
    return settings.profile
end

-- Fill an existing service's destination once, without replacing edits in the dialog.
function Bridge.restoreSettings(settings)
    if (settings.endpoint or '') ~= '' or (settings.bucket or '') ~= '' or (settings.publicBaseUrl or '') ~= '' then return end
    local name = settings.profile
    if not name or name == '' then return end
    local previous = {}
    for _, key in ipairs { 'catalogPath', 'spoolLimitMiB', 'maxFileMiB', 'historyKeep', 'graceDays' } do
        previous[key] = settings[key]
    end
    local ok, profile = LrTasks.pcall(function() return Bridge.call { 'info', '--profile', name } end)
    if not ok or settings.profile ~= name or (settings.endpoint or '') ~= '' or
        (settings.bucket or '') ~= '' or (settings.publicBaseUrl or '') ~= '' then return end
    for _, key in ipairs { 'endpoint', 'bucket', 'publicBaseUrl', 'catalogPath', 'historyKeep', 'graceDays' } do
        if settings[key] == previous[key] or key == 'endpoint' or key == 'bucket' or key == 'publicBaseUrl' then
            settings[key] = profile[key]
        end
    end
    if settings.spoolLimitMiB == previous.spoolLimitMiB then
        settings.spoolLimitMiB = profile.spoolLimitBytes / (1024 * 1024)
    end
    if settings.maxFileMiB == previous.maxFileMiB then
        settings.maxFileMiB = profile.maxFileBytes / (1024 * 1024)
    end
end
local function positiveInteger(settings, key, default)
    local value = settings[key]
    if value == nil then value = default end
    value = tonumber(value)
    assert(value and value > 0 and value < math.huge and value == math.floor(value),
        'Enter a positive whole number for ' .. key)
    return value
end
function Bridge.saveSettings(settings)
    Defaults.apply(settings)
    Bridge.ensureProfile(settings)
    Bridge.restoreSettings(settings)
    local access = settings.accessKeyId or ''
    local secret = settings.secretAccessKey or ''
    assert(access:find('%S') and secret:find('%S'), 'Enter both R2 access key ID and secret access key')
    for _, key in ipairs { 'endpoint', 'bucket', 'publicBaseUrl' } do
        assert(settings[key] and settings[key]:find('%S'), 'Complete the R2 ' .. key .. ' setting in Lightroom')
    end
    if not settings.catalogPath or settings.catalogPath == '' then
        settings.catalogPath = LrApplication.activeCatalog():getPath()
    end
    return Bridge.call({ 'configure', '--profile', settings.profile }, {
        endpoint = settings.endpoint, bucket = settings.bucket, publicBaseUrl = settings.publicBaseUrl,
        catalogPath = settings.catalogPath, accessKeyId = access, secretAccessKey = secret,
        spoolLimitBytes = positiveInteger(settings, 'spoolLimitMiB', Defaults.spoolLimitMiB) * 1024 * 1024,
        maxFileBytes = positiveInteger(settings, 'maxFileMiB', Defaults.maxFileMiB) * 1024 * 1024,
        historyKeep = positiveInteger(settings, 'historyKeep', Defaults.historyKeep),
        graceDays = positiveInteger(settings, 'graceDays', Defaults.graceDays),
    })
end
function Bridge.profile(settings)
    return Bridge.saveSettings(settings)
end
function Bridge.testConnection(settings)
    Bridge.saveSettings(settings)
    return Bridge.call { 'doctor', '--profile', settings.profile }
end
function Bridge.prepare(settings, gallery, title)
    Bridge.saveSettings(settings)
    return Bridge.call { 'init', '--profile', settings.profile, '--gallery', gallery,
        '--title', title, '--catalog', LrApplication.activeCatalog():getPath() }
end
function Bridge.stage(settings, prepared, id, path, rendition)
    return Bridge.call { 'stage', '--profile', settings.profile, '--job-id', prepared.job.jobId,
        '--photo', id, '--file', path, '--rendition', rendition or '' }.path
end
function Bridge.run(prepared, progress)
    Bridge.write(prepared.jobPath, prepared.job)
    local done = false
    local portion = 0.4
    if progress then
        progress:setPortionComplete(portion)
        progress:setCaption('Checking gallery')
        -- Work counts within each phase, not an elapsed-time estimate. Leave
        -- the final percent for the successful helper result and Lightroom.
        local phases = {
            preparing = {0.4, 0, 'Checking gallery'},
            inspecting = {0.4, 0.1, 'Inspecting photos'},
            uploading = {0.5, 0.35, 'Uploading JPEGs'},
            verifying = {0.85, 0.1, 'Verifying JPEGs'},
            committing = {0.95, 0.01, 'Saving gallery'},
            cleanup = {0.96, 0.03, 'Cleaning up previous versions'},
        }
        LrTasks.startAsyncTask(function()
            while not done do
                if progress:isCanceled() then
                    local f = io.open(LrPathUtils.child(prepared.directory, 'cancel'), 'w')
                    if f then f:write('cancel'); f:close() end
                    return
                end
                local ok, p = LrTasks.pcall(Bridge.read, LrPathUtils.child(prepared.directory, 'progress.json'))
                if done then return end -- A yielding read may outlive the helper.
                local phase = ok and type(p) == 'table' and phases[p.phase]
                if phase and type(p.completed) == 'number' and type(p.total) == 'number'
                    and p.completed >= 0 and p.total >= 0 and p.completed <= p.total then
                    local fraction = p.total > 0 and p.completed / p.total or 0
                    portion = math.max(portion, phase[1] + phase[2] * fraction)
                    progress:setPortionComplete(portion)
                    progress:setCaption(phase[3] .. (p.total > 0 and (' — ' .. p.completed .. ' of ' .. p.total) or ''))
                end
                LrTasks.sleep(0.25)
            end
        end)
    end
    local ok, result = LrTasks.pcall(function() return Bridge.runHelper { 'run', '--job', prepared.jobPath } end)
    done = true
    if not ok then error(result) end
    assert(result.status == 'committed', 'Gallery did not commit; photos remain pending')
    if progress then progress:setPortionComplete(0.99) end
    return result
end
return Bridge
