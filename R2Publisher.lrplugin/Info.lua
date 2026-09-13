return {
    LrSdkVersion = 15.0,
    LrSdkMinimumVersion = 6.0,
    LrToolkitIdentifier = 'com.jkeane.r2publisher',
    LrPluginName = 'R2 Publisher',
    LrPluginInfoUrl = 'https://github.com/jonkeane/photo-site',
    LrExportServiceProvider = {
        title = 'Cloudflare R2',
        file = 'PublishServiceProvider.lua',
    },
    LrMetadataProvider = 'MetadataDefinition.lua',
    VERSION = { major = 0, minor = 1, revision = 1, build = 3 },
}
