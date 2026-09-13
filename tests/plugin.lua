package.path = 'R2Publisher.lrplugin/?.lua;' .. package.path
-- Match the restricted os namespace documented in the SDK Guide, pp. 19-20.
-- Keep host-only functions private to the SDK doubles, never in plugin globals.
local hostExecute, hostRemove = os.execute, os.remove
os = { clock = os.clock, date = os.date, time = os.time, tmpname = os.tmpname }
local inWriteAccess = false
local catalog = { withWriteAccessDo = function(self, name, callback)
    inWriteAccess = true
    callback()
    inWriteAccess = false
end }
local catalogPath = '/test/catalog.lrcat'
function catalog:getPath() return catalogPath end
local services = {
    LrApplication = { activeCatalog = function() return catalog end },
    LrTasks = { pcall = pcall, execute = function(command)
        assert(not inWriteAccess, 'External process started during catalog write access')
        local ok, _, status = hostExecute(command)
        if type(ok) == 'number' then return ok end -- Lua 5.1
        return ok and 0 or status
    end },
    LrFileUtils = {
        exists = function(path)
            local f = io.open(path, 'rb')
            if f then f:close(); return 'file' end
            return false
        end,
        readFile = function(path)
            local f = assert(io.open(path, 'rb'))
            local data = f:read('*a'); f:close(); return data
        end,
        delete = hostRemove,
    },
    LrPathUtils = { child = function(a, b) return a .. '/' .. b end,
        getStandardFilePath = function(which)
            assert(which == 'temp')
            return '/test/lightroom-temp'
        end },
    LrView = { bind = function(key) return key end },
    LrDialogs = {},
    LrHttp = {},
}
function import(name) return assert(services[name], 'Unexpected SDK import: ' .. name) end
_PLUGIN = { path = 'R2Publisher.lrplugin' }
local Identity = require 'PhotoIdentity'
local Bridge = require 'JobBridge'
local Json = require 'Json'
local function photo(localID, props)
    return {
        localIdentifier = localID, catalog = catalog, props = props or {},
        getPropertyForPlugin = function(self, plugin, key) return self.props[key] end,
        setPropertyForPlugin = function(self, plugin, key, value) self.props[key] = value end,
        getFormattedMetadata = function(self, key) return ({title='日本 "quotes"', keywordTagsForExport='Film, Nature', preservedFileName='original.tif'})[key] or '' end,
        getRawMetadata = function() return '2026-09-09T12:00:00' end,
    }
end
local original = photo(1)
local id = Identity.get(original)
assert(Identity.get(original) == id, 'rename/republish changed identity')
local copy = photo(2, { photoUUID=id, identityOwner='1' })
assert(Identity.get(copy) ~= id, 'virtual copy retained master identity')
assert(Identity.gallery({serviceId='service'}, {localIdentifier=3}, {localIdentifier=4}) == 'service-3-4')
local firstService, secondService = {}, {profile=''}
local firstProfile = Bridge.ensureProfile(firstService)
local secondProfile = Bridge.ensureProfile(secondService)
assert(firstProfile ~= '' and firstProfile ~= secondProfile, 'New services share a storage identity')
assert(Bridge.ensureProfile(firstService) == firstProfile, 'Service identity changed on reuse')
assert(Bridge.ensureProfile({profile=firstProfile}) == firstProfile, 'Saved identity changed on reload')
assert(Bridge.ensureProfile({profile='website'}) == 'website', 'Existing destination identity changed')
local realUUID = Bridge.uuid
local concurrentSettings = {}
Bridge.uuid = function()
    concurrentSettings.profile = firstProfile
    return secondProfile
end
assert(Bridge.ensureProfile(concurrentSettings) == firstProfile, 'Concurrent initialization replaced identity')
Bridge.uuid = realUUID
assert(Bridge.quote("a'b $(touch nope)") == "'a'\\''b $(touch nope)'", 'unsafe shell quoting')
local text = '日本 "caption"\n<script>&'
assert(Json.decode(Json.encode({caption=text})).caption == text)
assert(Json.encode({}) == '[]')

-- Exercise the actual job bridge with os.rename absent, including replacement
-- of init's existing job and paths containing Unicode and shell metacharacters.
local reserved = os.tmpname()
assert(Bridge.read(reserved) == nil, 'Empty response file should have no result')
hostRemove(reserved)
local jobPath = reserved .. " 日本 'quoted' $(false) `false`.json"
Bridge.write(jobPath, {photos={}})
Bridge.write(jobPath, {photos={{id=id}}, title=text})
assert(Bridge.read(jobPath).photos[1].id == id)
assert(Bridge.read(jobPath).title == text)
assert(not services.LrFileUtils.exists(jobPath .. '.tmp'))
local execute = services.LrTasks.execute
services.LrTasks.execute = function() return 1 end
local saved, saveError = pcall(Bridge.write, jobPath, {title='failed replacement'})
assert(not saved and saveError:find('Cannot save job file', 1, true))
assert(Bridge.read(jobPath).title == text, 'Failed replacement damaged the existing job')
assert(Bridge.read(jobPath .. '.tmp').title == 'failed replacement')
local fileExists = services.LrFileUtils.exists
services.LrFileUtils.exists = function(path)
    if path == _PLUGIN.path .. '/bin/r2publisher' then return 'file' end
    return fileExists(path)
end
local helperOK, helperError = pcall(Bridge.call, {'info', '--profile', 'test'})
assert(not helperOK and helperError:find('R2 helper failed', 1, true), 'Missing response hid helper failure')
services.LrFileUtils.exists = fileExists
services.LrTasks.execute = execute
local actualCall = Bridge.call
local actualRunHelper = Bridge.runHelper
assert(type(Bridge.runHelper) == 'function', 'Bridge.runHelper API missing')
local runCalled = false
Bridge.runHelper = function(args, input)
    runCalled = true
    assert(type(args) == 'table' and args[1] == 'run', 'runHelper should accept helper args and stdin')
    assert(input == nil or type(input) == 'table', 'runHelper should accept a structured input object')
    return {status='committed'}
end
Bridge.call = function()
    assert(false, 'Legacy Bridge.call should remain compatible but Bridge.run should route through runHelper')
end
services.LrTasks.execute = function() return 1 end
assert(not pcall(Bridge.run, {jobPath=jobPath, job={photos={}}}))
assert(not runCalled, 'Helper ran after job save failure')
services.LrTasks.execute = execute
assert(Bridge.run({jobPath=jobPath, job={photos={{id=id}}}}).status == 'committed')
assert(runCalled and Bridge.read(jobPath).photos[1].id == id)

-- Poll the real bridge while a helper reports phases, including missing and
-- malformed snapshots. Progress must not complete until catalog acknowledgment.
local realRead = Bridge.read
local monitor, snapshot, lastPortion, lastCaption
services.LrTasks.startAsyncTask = function(action) monitor = coroutine.create(action) end
services.LrTasks.sleep = function() coroutine.yield() end
Bridge.read = function(path)
    if path == '/progress-test/progress.json' then return snapshot end
    return realRead(path)
end
local scope = {
    isCanceled = function() return false end,
    setCaption = function(_, caption) lastCaption = caption end,
    setPortionComplete = function(_, portion)
        assert(portion >= (lastPortion or 0) and portion < 1, 'Helper progress regressed or completed early')
        lastPortion = portion
    end,
}
local function poll(value)
    snapshot = value
    local ok, problem = coroutine.resume(monitor)
    assert(ok, problem)
end
Bridge.runHelper = function()
    poll(nil)
    poll(true)
    poll({phase='uploading',completed='bad',total=3})
    assert(lastPortion == 0.4 and lastCaption == 'Checking gallery')
    for _, phase in ipairs({'inspecting','uploading','verifying','committing','cleanup'}) do
        poll({phase=phase,completed=0,total=3})
        poll({phase=phase,completed=1,total=3})
        assert(lastCaption:find('1 of 3',1,true))
        poll({phase=phase,completed=3,total=3})
    end
    return {status='committed'}
end
Bridge.run({jobPath=jobPath,directory='/progress-test',job={photos={}}}, scope)
assert(lastPortion == 0.99)
poll({phase='uploading',completed=0,total=3})
assert(coroutine.status(monitor) == 'dead', 'Progress monitor outlived the helper')
Bridge.runHelper = function() error('network failure') end
lastPortion = nil
assert(not pcall(Bridge.run, {jobPath=jobPath,directory='/progress-test',job={photos={}}}, scope))
poll(nil)
assert(coroutine.status(monitor) == 'dead' and lastPortion == 0.4)
Bridge.read = realRead
services.LrTasks.startAsyncTask = nil
services.LrTasks.sleep = nil
Bridge.runHelper = actualRunHelper
Bridge.call = actualCall
hostRemove(jobPath); hostRemove(jobPath .. '.tmp')

-- Credentials travel through private stdin files, never shell arguments or jobs.
local credentialInput, credentialDirectory, helperFailure
services.LrTasks.execute = function(command)
    assert(not command:find('test-secret', 1, true) and not command:find('test-access', 1, true),
        'Credentials leaked into process arguments')
    if command:find('/bin/r2publisher', 1, true) then
        local path = command:match(" < '([^']+)'$")
        assert(path, 'Credential input missing')
        credentialInput = path
        credentialDirectory = path:match('^(.*)/input.json$')
        assert(execute('test "$(/usr/bin/stat -f %Lp ' .. Bridge.quote(credentialDirectory) .. ')" = 700') == 0,
            'Credential directory was not private')
        local keys = Bridge.read(path)
        assert(keys.accessKeyId == 'test-access' and keys.secretAccessKey == "test-secret ' $(false)")
        if helperFailure then error('simulated helper execution failure') end
        local response = assert(command:match(" %-%-out '([^']+)'"))
        local f = assert(io.open(response, 'wb'))
        f:write(Json.encode({status='saved'})); f:close()
        return 0
    end
    return execute(command)
end
local authSettings = {profile='test', accessKeyId='test-access', secretAccessKey="test-secret ' $(false)",
    endpoint='https://example.r2.cloudflarestorage.com', bucket='photos', publicBaseUrl='https://images.example.com'}
assert(Bridge.saveSettings(authSettings).status == 'saved')
assert(not fileExists(credentialInput) and execute('test ! -d ' .. Bridge.quote(credentialDirectory)) == 0,
    'Credential input was not removed')
helperFailure = true
assert(not pcall(Bridge.saveSettings, authSettings))
assert(not fileExists(credentialInput) and execute('test ! -d ' .. Bridge.quote(credentialDirectory)) == 0,
    'Failed helper retained credential input')
services.LrTasks.execute = execute
local authCalls = {}
Bridge.call = function(args, input)
    authCalls[#authCalls+1] = {command=args[1], input=input, args=args}
    return {status='ok'}
end
assert(not pcall(Bridge.saveSettings, {profile='test', endpoint='https://example.r2.cloudflarestorage.com'}))
assert(#authCalls == 0, 'Missing keys should prevent saving')
assert(not pcall(Bridge.saveSettings, {profile='test',endpoint='https://example.r2.cloudflarestorage.com',accessKeyId='only-one'}))
Bridge.prepare(authSettings, 'gallery', 'Gallery')
assert(authCalls[1].command == 'configure' and authCalls[2].command == 'init')
assert(authCalls[1].input.secretAccessKey == authSettings.secretAccessKey and authCalls[2].input == nil)
Bridge.testConnection(authSettings)
assert(authCalls[3].command == 'configure' and authCalls[4].command == 'doctor')
assert(authCalls[1].input.catalogPath == catalogPath)
assert(authCalls[1].input.spoolLimitBytes == 5 * 1024 * 1024 * 1024)
assert(authCalls[1].input.maxFileBytes == 100 * 1024 * 1024)
assert(authCalls[1].input.historyKeep == 1 and authCalls[1].input.graceDays == 30)
authSettings.spoolLimitMiB = '2048'
authSettings.maxFileMiB = '75'
authSettings.historyKeep = '5'
authSettings.graceDays = '7'
Bridge.profile(authSettings)
assert(authCalls[5].input.spoolLimitBytes == 2048 * 1024 * 1024)
assert(authCalls[5].input.maxFileBytes == 75 * 1024 * 1024)
assert(authCalls[5].input.historyKeep == 5 and authCalls[5].input.graceDays == 7)
local headlessSettings = {}
for key, value in pairs(authSettings) do headlessSettings[key] = value end
headlessSettings.profile = nil
Bridge.saveSettings(headlessSettings)
assert(headlessSettings.profile and headlessSettings.profile ~= 'test')
assert(authCalls[6].args[3] == headlessSettings.profile, 'Publishing did not use its automatic storage ID')
for _, invalid in ipairs {'', 'abc', 0, -1, 1.5, math.huge} do
    authSettings.spoolLimitMiB = invalid
    assert(not pcall(Bridge.saveSettings, authSettings), 'Invalid storage limit accepted')
end
local oldProfile = {endpoint='https://existing.r2.cloudflarestorage.com', bucket='existing',
    publicBaseUrl='https://existing.example.com', catalogPath='/original/catalog.lrcat',
    spoolLimitBytes=1024*1024*2048, maxFileBytes=1024*1024*75, historyKeep=4, graceDays=8}
Bridge.call = function() return oldProfile end
local oldSettings = {profile='test'}
Bridge.restoreSettings(oldSettings)
assert(oldSettings.bucket == 'existing' and oldSettings.catalogPath == '/original/catalog.lrcat')
assert(oldSettings.spoolLimitMiB == 2048 and oldSettings.maxFileMiB == 75)
oldSettings.bucket = 'edited'
Bridge.restoreSettings(oldSettings)
assert(oldSettings.bucket == 'edited', 'Saved profile overwrote dialog edits')
Bridge.call = actualCall

local Metadata = require 'PublicMetadata'
local metadata = Metadata.read(original, {ownerName=''})
assert(metadata.license == '', 'Public license must remain optional')
assert(metadata.dateTaken == nil and metadata.exif.preservedfilename == 'original.tif')
for key in pairs(metadata.exif) do assert(key == 'preservedfilename', 'Catalog technical EXIF leaked into job') end
local descriptiveOnly = {
    getFormattedMetadata = function(_, key)
        assert(key ~= 'cameraModel' and key ~= 'lens' and key ~= 'focalLength' and key ~= 'aperture'
            and key ~= 'shutterSpeed' and key ~= 'isoSpeedRating' and key ~= 'flash', 'Read catalog technical EXIF')
        return ''
    end,
    getRawMetadata = function() error('Read catalog capture time') end,
}
Metadata.read(descriptiveOnly, {ownerName=''})

local secondaryExports, secondaryFailure, secondaryHook = {}, nil, nil
local childScopes = {}
services.LrProgressScope = function(args)
    local parent = assert(args.parent, 'Rendering must use a child of the publish scope')
    assert(not parent.activeChild, 'Rendering scopes overlap')
    local start = parent.portion or 0
    local finish = args.parentEndRange
    assert(finish >= start and finish <= 0.4 + 1e-12, 'Render can escape the first 40%')
    local scope = {parent=parent, finish=finish}
    function scope:isCanceled() return self.cancelled or parent:isCanceled() end
    function scope:setCaption(caption) parent:setCaption(caption) end
    function scope:setPortionComplete(portion)
        assert(not self.finished, 'Renderer updated a finished scope')
        parent.childUpdate = true
        parent:setPortionComplete(start + (finish - start) * portion)
        parent.childUpdate = nil
    end
    function scope:done()
        self:setPortionComplete(1)
        self.finished = true
        parent.activeChild = nil
    end
    parent.activeChild = scope
    childScopes[#childScopes + 1] = scope
    return scope
end
local removedSecondary = {}
local actualDelete = services.LrFileUtils.delete
services.LrFileUtils.delete = function(path)
    removedSecondary[path] = true
    return actualDelete(path)
end
services.LrExportSession = function(args)
    local settings = args.exportSettings
    assert(#args.photosToExport == 1 and args.photosToExport[1] == original)
    assert(settings.LR_format == 'JPEG' and settings.LR_export_colorSpace == 'sRGB')
    assert(settings.LR_size_resizeType == 'shortEdge' and settings.LR_size_units == 'pixels')
    assert(settings.LR_size_doConstrain and settings.LR_size_doNotEnlarge)
    assert(settings.LR_export_destinationType == 'tempFolder' and settings.LR_reimportExportedPhoto == false)
    assert(settings.LR_export_destinationPathPrefix == '/test/lightroom-temp',
        'export settings are missing the LR_export_destinationPathPrefix')
    assert(settings.LR_publishService == nil and settings.LR_jpeg_useLimitSize == false)
    secondaryExports[#secondaryExports + 1] = settings
    local edge = settings.LR_size_maxHeight
    return {renditions = function(_, options)
        local scope = assert(options and options.progressScope, 'Secondary rendering needs an explicit child scope')
        assert(options.renderProgressPortion == 1 and options.stopIfCanceled)
        scope:setPortionComplete(0)
        local returned = false
        return function()
            if returned then return end
            returned = true
            return 1, {waitForRender = function()
                -- Exercise the native renderer reporting nearly/full completion.
                -- These values must stay inside this JPEG's parent allocation.
                scope:setPortionComplete(0.95)
                scope:setPortionComplete(1)
                if secondaryHook then secondaryHook(scope) end
                return edge ~= secondaryFailure, edge == secondaryFailure and 'secondary failure' or '/secondary-' .. edge .. '.jpg'
            end}
        end
    end}
end
local Provider = require 'ExportServiceProvider'
local sizeSettings = {LR_size_doConstrain=false, LR_size_maxHeight=100, LR_jpeg_quality=0.9}
Provider.updateExportSettings(sizeSettings)
assert(sizeSettings.LR_size_maxHeight == 4096 and sizeSettings.LR_size_resizeType == 'longEdge')
assert(sizeSettings.LR_jpeg_quality == 0.9 and sizeSettings.LR_size_doNotEnlarge)
for _, edge in ipairs({2048, '6000'}) do
    sizeSettings.fullSizeLongEdge = edge
    Provider.updateExportSettings(sizeSettings)
    assert(sizeSettings.LR_size_maxHeight == tonumber(edge) and sizeSettings.LR_size_maxWidth == tonumber(edge))
    assert(sizeSettings.LR_size_resizeType == 'longEdge' and sizeSettings.LR_size_doNotEnlarge)
    assert(sizeSettings.fullSizeLongEdge == edge, 'Rendering mutated the full-size preference')
end
sizeSettings.fullSizeLongEdge = nil
Provider.updateExportSettings(sizeSettings)
assert(sizeSettings.LR_size_maxHeight == 4096, 'Custom full size leaked into the default')
local Render = require 'Renditions'
local Settings = require 'Settings'
local defaults = {}
for _, field in ipairs(Settings.fields) do defaults[field.key] = field.default end
assert(defaults.thumbnailShortEdge == 256 and defaults.galleryShortEdge == 1024)
assert(defaults.fullSizeLongEdge == 4096)
assert(defaults.endpoint == '' and defaults.bucket == '' and defaults.publicBaseUrl == '')
assert(defaults.catalogPath == '' and defaults.spoolLimitMiB == 5120 and defaults.maxFileMiB == 100)
assert(defaults.historyKeep == 1 and defaults.graceDays == 30)
local Defaults = require 'Defaults'
assert(Defaults.historyKeep == 1 and Defaults.graceDays == 30)
local originalAsync = services.LrTasks.startAsyncTask
services.LrTasks.startAsyncTask = function(fn) fn() end
Bridge.call = function() error('profile not found') end
local freshSettings = {profile=''}
Provider.startDialog(freshSettings)
assert(freshSettings.catalogPath == catalogPath, 'New service did not select the active catalog')
assert(freshSettings.profile ~= '', 'New dialog did not initialize its storage ID')
local freshProfile = freshSettings.profile
Provider.startDialog(freshSettings)
assert(freshSettings.profile == freshProfile, 'Reopening settings replaced the storage ID')
Bridge.call = function() return oldProfile end
local restoredSettings = {profile='existing'}
Provider.startDialog(restoredSettings)
assert(restoredSettings.catalogPath == '/original/catalog.lrcat', 'Existing catalog binding was replaced')
assert(restoredSettings.bucket == 'existing' and restoredSettings.profile == 'existing')
Bridge.call = actualCall
services.LrTasks.startAsyncTask = originalAsync
for _, invalid in ipairs({'', 'abc', 0, -1, 1.5, math.huge, 0/0}) do
    assert(not Render.validateEdge(nil, invalid), 'Invalid edge accepted')
    local ok, problem = pcall(Provider.updateExportSettings, {fullSizeLongEdge=invalid})
    assert(not ok and problem:find('Full size long edge:', 1, true), 'Invalid full size accepted')
end
local valid, parsed = Render.validateEdge(nil, '2048')
assert(valid and parsed == 2048)
local choices = {LR_jpeg_quality=0.7, LR_removeLocationMetadata=true, LR_useWatermark=true,
    LR_watermarking_id='preset', LR_outputSharpeningOn=true, LR_outputSharpeningMedia='screen', LR_outputSharpeningLevel=2,
    LR_publishService='must not recurse', LR_size_maxHeight=4096,
    LR_export_destinationPathPrefix='/original-destination'}
local childSettings = Render.settings(choices, 256)
assert(childSettings.LR_removeLocationMetadata and childSettings.LR_useWatermark and childSettings.LR_watermarking_id == 'preset')
assert(childSettings.LR_jpeg_quality == 0.7 and childSettings.LR_outputSharpeningLevel == 2)
assert(choices.LR_size_maxHeight == 4096 and childSettings ~= choices)
assert(choices.LR_export_destinationPathPrefix == '/original-destination')

local resultStatus = 'committed'
local resultWarnings
Bridge.profile = function() return {serviceId='service'} end
Bridge.prepare = function() return {job={photos={},removals={}},jobPath='/job'} end
Bridge.stage = function(_, _, photoId, _, name) return '/staged/' .. photoId .. (name and '.' .. name or '') .. '.jpg' end
Bridge.run = function(prepared)
    if resultStatus == 'failed' then error('network failure') end
    local photos = {}
    for _, p in ipairs(prepared.job.photos) do
        assert(p.renditions.thumbnail ~= p.path and p.renditions.gallery ~= p.path and p.renditions.gallery ~= p.renditions.thumbnail)
        photos[#photos+1] = { id=p.id, status='committed', url='https://images.example.com/'..p.id }
    end
    return {photos=photos,manifestUrl='https://images.example.com/current.json',warnings=resultWarnings}
end
local function context(renderOK, photoCount)
    photoCount = photoCount or 1
    local primaryScope, primaryIndex
    local r = { photo=original, failed=false, recorded=false,
        waitForRender=function()
            if primaryScope then
                primaryScope:setPortionComplete((primaryIndex - 0.05) / photoCount)
                primaryScope:setPortionComplete(primaryIndex / photoCount)
            end
            return renderOK, renderOK and '/rendered.jpg' or 'render error'
        end,
        uploadFailed=function(self) self.failed=true end,
        recordPublishedPhotoId=function(self) self.recorded=true end,
        recordPublishedPhotoUrl=function() end,
    }
    local c
    c = {propertyTable={profile='test',ownerName=''},publishedCollection={localIdentifier=4},publishService={localIdentifier=3},publishedCollectionInfo={name='Gallery'},
        configureProgress=function(self, options)
            assert(type(options.renderPortion) == 'number'
                and options.renderPortion > 0 and options.renderPortion < 1,
                'configureProgress: args.renderPortion must be a number between 0 and 1')
            self.renderPortion = options.renderPortion
            self.progress = {isCanceled=function() return self.cancelled end,
                setCaption=function(_, caption) self.caption=caption end,
                setPortionComplete=function(scope, portion)
                    assert(not scope.activeChild or scope.childUpdate, 'Parent written while a child renderer owns progress')
                    assert(portion + 1e-12 >= (self.portion or 0), 'Publish progress moved backwards')
                    if portion == 1 then assert(r.recorded, 'Progress completed before acknowledgment') end
                    self.portion=portion
                    scope.portion=portion
                    self.portions = self.portions or {}
                    self.portions[#self.portions + 1] = portion
                end,
                done=function() self.done=true end}
            return self.progress
        end,
        renditions=function(self, options)
            -- Model the observed context-managed primary render flashing the
            -- shared publish scope to 100%, despite configureProgress's fraction.
            self.progress:setPortionComplete(1)
            error('Primary rendering must use an explicit bounded child scope')
        end,
        startRendering=function() error('Do not start an independently managed render pipeline') end,
        exportSession={countRenditions=function() return photoCount end,
            renditions=function(_, options)
                primaryScope = assert(options and options.progressScope, 'Primary rendering needs a child scope')
                assert(primaryScope.parent == c.progress and primaryScope.finish == c.renderPortion)
                assert(options.renderProgressPortion == 1 and options.stopIfCanceled)
                c.started=true
                primaryScope:setPortionComplete(0)
                local n=0
                return function()
                    if c.cancelled or primaryScope:isCanceled() then return end
                    n=n+1
                    if n <= photoCount then primaryIndex=n; return n,r end
                    c.primaryFinished=true
                end
            end,
            recordRemoteCollectionId=function() end,recordRemoteCollectionUrl=function() end},
    }
    return c,r
end
local c,r=context(true);Provider.processRenderedPhotos({},c);assert(r.recorded and not r.failed)
assert(#secondaryExports == 2 and secondaryExports[1].LR_size_maxHeight == 256 and secondaryExports[2].LR_size_maxHeight == 1024)
assert(removedSecondary['/secondary-256.jpg'] and removedSecondary['/secondary-1024.jpg'])
c,r=context(true)
c.propertyTable.thumbnailShortEdge=512
c.propertyTable.galleryShortEdge='2048'
Provider.processRenderedPhotos({},c)
assert(r.recorded and not r.failed)
assert(secondaryExports[3].LR_size_maxHeight == 512 and secondaryExports[4].LR_size_maxHeight == 2048)
assert(removedSecondary['/secondary-512.jpg'] and removedSecondary['/secondary-2048.jpg'])
assert(c.propertyTable.galleryShortEdge == '2048', 'Rendering mutated publisher settings')
c,r=context(true);c.propertyTable.thumbnailShortEdge=0
assert(not pcall(Provider.processRenderedPhotos,{},c) and r.failed and not r.recorded)
c,r=context(true);Provider.processRenderedPhotos({},c)
assert(secondaryExports[5].LR_size_maxHeight == 256 and secondaryExports[6].LR_size_maxHeight == 1024,
    'Custom sizes leaked into a different publish service')
secondaryFailure=1024;c,r=context(true);assert(not pcall(Provider.processRenderedPhotos,{},c));assert(r.failed and not r.recorded)
secondaryFailure=nil;c,r=context(true);secondaryHook=function() c.cancelled=true end
assert(not pcall(Provider.processRenderedPhotos,{},c));assert(r.failed and not r.recorded)
secondaryHook=nil
local savedStage=Bridge.stage
Bridge.stage=function(_, _, _, _, name) if name then error('staging failed') end return '/primary.jpg' end
removedSecondary['/secondary-256.jpg']=nil
c,r=context(true);assert(not pcall(Provider.processRenderedPhotos,{},c));assert(r.failed and not r.recorded and removedSecondary['/secondary-256.jpg'])
Bridge.stage=savedStage
resultStatus='failed';c,r=context(true);assert(not pcall(Provider.processRenderedPhotos,{},c));assert(r.failed and not r.recorded,'failed upload marked published')
resultStatus='committed';c,r=context(false);assert(not pcall(Provider.processRenderedPhotos,{},c));assert(r.failed and not r.recorded,'failed rendering marked published')

-- Primary rendering finishes before secondary sessions start. Both primary and
-- secondary renderers reporting 100% must stay inside their bounded shares.
for _, photoCount in ipairs({1, 4}) do
    c,r=context(true, photoCount)
    local stagedCount, observedScopes = 0, #childScopes
    Bridge.stage=function(...)
        stagedCount = stagedCount + 1
        if stagedCount <= photoCount then
            assert(not c.primaryFinished and c.portion <= c.renderPortion)
        else
            assert(c.primaryFinished, 'Secondary rendering overlaps the primary pipeline')
            assert(c.portion <= 0.4 * stagedCount / (photoCount * 3) + 1e-12,
                'Secondary renderer advanced beyond its JPEG allocation')
        end
        return savedStage(...)
    end
    Provider.processRenderedPhotos({},c)
    assert(c.started, 'Primary rendering pipeline was not started')
    assert(stagedCount == photoCount * 3)
    assert(#childScopes - observedScopes == 1 + photoCount * 2)
    assert(c.portions[1] == 0 and c.portions[#c.portions] == 1)
    for i=1,#c.portions - 1 do
        assert(c.portions[i] <= 0.4 + 1e-12, 'Rendering completed publication early')
    end
    assert(math.abs(c.portions[#c.portions-1] - 0.4) < 1e-12)
end
Bridge.stage=savedStage

-- A failed stage must not start the next secondary render or publish. Cancelling
-- a primary render must stop before staging or starting secondary exports.
Bridge.stage=function(_, _, _, _, name)
    if name then error('staging failed') end
    return '/primary.jpg'
end
c,r=context(true, 4)
local scopesBefore = #childScopes
assert(not pcall(Provider.processRenderedPhotos,{},c))
assert(#childScopes == scopesBefore + 2 and c.portion < 0.4)
Bridge.stage=savedStage
c,r=context(true)
r.waitForRender=function() c.cancelled=true; return true,'/rendered.jpg' end
local exportsBefore = #secondaryExports
assert(not pcall(Provider.processRenderedPhotos,{},c))
assert(r.failed and not r.recorded and c.portion <= c.renderPortion and #secondaryExports == exportsBefore)
-- The primary child must also be disposed when staging fails or that child is
-- cancelled directly. Neither case may start secondary rendering or upload.
c,r=context(true)
Bridge.stage=function() error('primary staging failed') end
assert(not pcall(Provider.processRenderedPhotos,{},c))
assert(r.failed and not r.recorded and c.portion <= c.renderPortion and #secondaryExports == exportsBefore)
Bridge.stage=savedStage
c,r=context(true)
r.waitForRender=function() c.progress.activeChild.cancelled=true; return true,'/rendered.jpg' end
local cancelledStageCalls=0
Bridge.stage=function(...) cancelledStageCalls=cancelledStageCalls+1; return savedStage(...) end
assert(not pcall(Provider.processRenderedPhotos,{},c))
assert(r.failed and not r.recorded and cancelledStageCalls == 0 and #secondaryExports == exportsBefore)
Bridge.stage=savedStage
-- Cancellation from a child scope also stops publication, and every scope is
-- disposed on success, render failure, stage failure, and cancellation.
c,r=context(true);secondaryHook=function(scope) scope.cancelled=true end
assert(not pcall(Provider.processRenderedPhotos,{},c) and r.failed and not r.recorded)
secondaryHook=nil
for _, scope in ipairs(childScopes) do assert(scope.finished, 'Leaked rendering progress scope') end

-- Cover warnings appear only after successful publication and progress cleanup.
local warningCalls = 0
services.LrDialogs.message = function(title, message, severity)
    warningCalls = warningCalls + 1
    assert(c.done and c.portion == 1 and r.recorded and not r.failed,
        'Cover warning appeared before export finished')
    assert(title == 'Export finished — gallery cover warning' and severity == 'warning')
    assert(message == 'Upload completed successfully.\n\n' .. table.concat(resultWarnings, '\n\n'))
end
for _, count in ipairs({0, 2, 3}) do
    resultWarnings = {'Gallery "Gallery" has ' .. count .. ' photos with the tag gallery-cover. Set the tag on exactly one photo and republish.'}
    c,r=context(true)
    Provider.processRenderedPhotos({},c)
    assert(r.recorded and not r.failed)
end
assert(warningCalls == 3)
resultWarnings = {}
c,r=context(true);Provider.processRenderedPhotos({},c)
assert(warningCalls == 3, 'Valid cover produced a warning')
resultWarnings = {'Must not display on failure'}
resultStatus='failed';c,r=context(true)
assert(not pcall(Provider.processRenderedPhotos,{},c) and r.failed and not r.recorded)
assert(warningCalls == 3, 'Failed upload displayed a cover warning')
resultStatus='committed';resultWarnings=nil

local Publish = require 'PublishServiceProvider'
assert(Publish.deleteFirstOnPublish() == true,
    'Pending removals must run before uploads that could fail ownership checks')
-- Collection dialogs are blocking callbacks; catalog reads must be deferred,
-- and the displayed ID must never become a persisted collection setting.
local pendingDialogTask, inDialogTask
services.LrTasks.startAsyncTask = function(task) pendingDialogTask = task end
local factory = {}
for _, kind in ipairs({'group_box', 'row', 'static_text', 'push_button'}) do
    factory[kind] = function(_, spec) return spec end
end
local function collectionDialog(collection)
    pendingDialogTask = nil
    local info = { pluginContext = {}, collectionSettings = {}, publishedCollection = collection }
    local view = Publish.viewForCollectionSettings(factory, {galleryId='export-only'}, info)
    assert(info.pluginContext.galleryId == '', 'Dialog read the catalog synchronously')
    assert(not info.pluginContext.canCopyGalleryId, 'Copy enabled before ID was loaded')
    assert(view.bind_to_object == info.pluginContext and view[1][2].selectable)
    if pendingDialogTask then
        inDialogTask = true
        pendingDialogTask()
        inDialogTask = false
    end
    assert(next(info.collectionSettings) == nil, 'Display state persisted as collection settings')
    return info.pluginContext, view[1][3]
end
local function publishedCollection(id)
    return { getRemoteId = function()
        assert(inDialogTask, 'Remote ID read outside an async task')
        return id
    end }
end
assert(collectionDialog(publishedCollection('saved-gallery-id')).galleryId == 'saved-gallery-id')
assert(collectionDialog(publishedCollection(123)).galleryId == '123')
for _, props in ipairs({collectionDialog(nil), collectionDialog(publishedCollection(nil)),
    (collectionDialog(publishedCollection('')))}) do
    assert(props.galleryId == '' and not props.canCopyGalleryId and props.galleryIdHelp:find('Publish this collection first', 1, true))
end
local dialogFailure = collectionDialog({getRemoteId=function() error('catalog unavailable') end})
assert(dialogFailure.galleryId == '' and dialogFailure.galleryIdHelp:find('Could not read', 1, true))
local copyProps, copyButton = collectionDialog(publishedCollection("gallery's %s $(false)"))
assert(copyProps.canCopyGalleryId and copyProps.galleryIdHelp == '')
local savedExecute = services.LrTasks.execute
local copyStatus = 0
services.LrTasks.execute = function(command)
    assert(command == "/usr/bin/printf %s 'gallery'\\''s %s $(false)' | /usr/bin/pbcopy", 'Unsafe clipboard command')
    if copyStatus == 'error' then error('process failed') end
    return copyStatus
end
for _, status in ipairs({0, 1, 'error'}) do
    copyStatus = status
    copyButton.action()
    assert(not copyProps.canCopyGalleryId, 'Copy enabled while clipboard task is running')
    pendingDialogTask()
    assert(copyProps.canCopyGalleryId, 'Copy not re-enabled after clipboard task')
    if status == 0 then
        assert(copyProps.galleryIdHelp == 'Copied.')
    else
        assert(copyProps.galleryIdHelp:find('Could not copy', 1, true))
    end
end
services.LrTasks.execute = savedExecute
services.LrTasks.startAsyncTask = nil

-- Simulate Lightroom's cooperative tasks. Every helper phase yields so other
-- galleries and collection callbacks try to start while a job is still active.
services.LrTasks.sleep = function() coroutine.yield('waiting') end
local activeGalleries, events, jobs = {}, {}, {}
Bridge.profile = function()
    return {serviceId='service'}
end
Bridge.prepare = function(_, gallery, title)
    assert(not activeGalleries[gallery], 'Overlapping preparation of the same gallery')
    activeGalleries[gallery] = true
    local count = 0
    for _ in pairs(activeGalleries) do count = count + 1 end
    assert(count <= 2, 'Too many active galleries')
    events[#events+1] = gallery
    coroutine.yield('prepare')
    local prepared = {job={galleryId=gallery,title=title,photos={},removals={}}, entries={{id='old-photo'}}}
    jobs[gallery] = prepared.job
    return prepared
end
Bridge.stage = function(_, prepared, photoId)
    assert(activeGalleries[prepared.job.galleryId], 'Staging outside active job')
    coroutine.yield('stage')
    return '/staged/' .. photoId .. '.jpg'
end
Bridge.run = function(prepared)
    assert(activeGalleries[prepared.job.galleryId], 'Upload outside active job')
    coroutine.yield('run')
    activeGalleries[prepared.job.galleryId] = nil
    if prepared.job.title == 'Fail' then error('network failure') end
    local photos = {}
    for _, p in ipairs(prepared.job.photos) do photos[#photos+1] = {id=p.id,status='committed',url='https://images.example.com/'..p.id} end
    return {photos=photos,manifestUrl='https://images.example.com/'..prepared.job.galleryId..'/current.json'}
end
local function task(action)
    local t = {}
    t.thread = coroutine.create(function() t.ok,t.result=pcall(action) end)
    return t
end
local function step(t)
    if coroutine.status(t.thread) ~= 'dead' then
        local ok, problem = coroutine.resume(t.thread)
        assert(ok, problem)
    end
end
local function drain(tasks)
    for _=1,100 do
        local pending = false
        for _,t in ipairs(tasks) do
            step(t)
            if coroutine.status(t.thread) ~= 'dead' then pending=true end
        end
        if not pending then return end
    end
    error('Publication queue did not drain')
end
local function galleryTask(gallery, title)
    local ctx, rendition = context(true)
    ctx.publishedCollectionInfo = {remoteId=gallery,name=title or gallery}
    ctx.exportSession.recordRemoteCollectionId = function(_, remoteId) ctx.remoteId=remoteId end
    return task(function() Publish.processRenderedPhotos({},ctx) end), ctx, rendition
end
local a,ca,ra = galleryTask('gallery-a')
local b,cb,rb = galleryTask('gallery-b')
local d,cd,rd = galleryTask('gallery-c')
local rename = task(function() Publish.renamePublishedCollection({}, {remoteId='rename',name='Renamed'}) end)
local sorted = task(function() Publish.imposeSortOrderOnPublishedCollection({}, {remoteCollectionId='sort',name='Sort'}, {'old-photo'}) end)
local deleted = task(function() Publish.deletePublishedCollection({}, {remoteId='delete',name='Delete'}) end)
function catalog:getPublishedCollectionByLocalIdentifier(localId)
    assert(localId == 7)
    return {getCollectionInfoSummary=function() return {remoteId='remove',name='Remove'} end}
end
local acknowledged
local removed = task(function()
    Publish.deletePhotosFromPublishedCollection({}, {'old-photo'}, function(photoId) acknowledged=photoId end, 7)
end)
step(a);step(b);step(d);step(rename);step(sorted);step(deleted);step(removed)
assert(#events == 2 and activeGalleries['gallery-a'] and activeGalleries['gallery-b'], 'Different galleries did not overlap')
assert(cd.caption:find('Waiting',1,true), 'Third gallery did not wait for a slot')
drain({a,b,d,rename,sorted,deleted,removed})
for _,t in ipairs({a,b,d,rename,sorted,deleted,removed}) do assert(t.ok,t.result) end
assert(table.concat(events,',') == 'gallery-a,gallery-b,gallery-c,rename,sort,delete,remove', 'Callbacks bypassed FIFO queue')
assert(ra.recorded and rb.recorded and rd.recorded and ca.done and cb.done and cd.done)
assert(ca.remoteId == 'gallery-a' and cb.remoteId == 'gallery-b' and cd.remoteId == 'gallery-c')
assert(jobs.rename.title == 'Renamed' and jobs.sort.order[1] == 'old-photo')
assert(jobs.delete.removals[1] == 'old-photo' and acknowledged == 'old-photo')
assert(jobs.remove.removals[1] == 'old-photo' and #jobs.remove.photos == 0 and #jobs.delete.photos == 0,
    'Removal callbacks must not submit photos for upload or ownership checks')

-- A failed active publish and a cancelled waiter must both release their place.
events,jobs = {},{}
local failed,cf,rf = galleryTask('failed','Fail')
local cancelled,cc,rc = galleryTask('failed','Cancelled')
local nextTask,cn,rn = galleryTask('failed','Next')
step(failed);step(cancelled);step(nextTask)
cc.cancelled=true
step(cancelled)
assert(cancelled.ok == false and cc.done and not rc.recorded)
assert(#events == 1, 'Cancelled waiter created a job')
drain({failed,nextTask})
assert(not failed.ok and rf.failed and not rf.recorded and cf.done)
assert(nextTask.ok and rn.recorded and cn.done, 'Failure left the queue locked')
assert(table.concat(events,',') == 'failed,failed')

-- Export, rename, sorting and deletion of the same gallery remain FIFO.
events,jobs = {},{}
local same,cs,rs = galleryTask('same')
local sameRename = task(function() Publish.renamePublishedCollection({}, {remoteId='same',name='Renamed'}) end)
local sameSort = task(function() Publish.imposeSortOrderOnPublishedCollection({}, {remoteCollectionId='same',name='Sorted'}, {'old-photo'}) end)
local sameDelete = task(function() Publish.deletePublishedCollection({}, {remoteId='same',name='Deleted'}) end)
step(same);step(sameRename);step(sameSort);step(sameDelete)
assert(#events == 1, 'Same-gallery mutation bypassed the active export')
drain({same,sameRename,sameSort,sameDelete})
for _,t in ipairs({same,sameRename,sameSort,sameDelete}) do assert(t.ok,t.result) end
assert(rs.recorded and jobs.same.title == 'Deleted' and jobs.same.removals[1] == 'old-photo')
assert(#events == 4)

-- Pending first exports and callbacks arriving with the newly assigned remote
-- ID must still address the same queue, even before all waiters have started.
events,jobs = {},{}
local firstContext = context(true)
local pendingContext = context(true)
local firstPublish = task(function() Publish.processRenderedPhotos({},firstContext) end)
local pendingPublish = task(function() Publish.processRenderedPhotos({},pendingContext) end)
step(firstPublish);step(pendingPublish)
local assignedGallery = events[1]
local afterAssignment = task(function()
    Publish.renamePublishedCollection({}, {remoteId=assignedGallery,name='After first publish'})
end)
step(afterAssignment)
assert(#events == 1, 'Remote ID assignment split the gallery queue')
drain({firstPublish,pendingPublish,afterAssignment})
for _,t in ipairs({firstPublish,pendingPublish,afterAssignment}) do assert(t.ok,t.result) end
assert(#events == 3 and jobs[assignedGallery].title == 'After first publish')

print('Plugin SDK sandbox, job saves, identity, publish queue, cancellation and acknowledgment tests passed')
