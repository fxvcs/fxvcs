fx_version 'cerulean'
game 'gta5'

description 'Declares FXServer capability requirements alongside a resource dependency'

dependencies {
    'mapmanager',                  -- a real resource dependency
    '/server:4500',                -- requires at least server build 4500
    '/policy:subdir_file_mapping', -- requires the server key to have 'subdir_file_mapping' granted
    '/onesync',                    -- requires state awareness to be enabled
    '/gameBuild:h4',               -- requires at least game build 2189
    '/native:0xE27C97A0',          -- requires native 0xE27C97A0 to be supported
    '/assetpacks',                 -- requires asset pack support
}

server_script 'server.lua'
