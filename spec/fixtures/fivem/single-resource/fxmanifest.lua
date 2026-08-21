fx_version 'cerulean'
game 'gta5'
lua54 'yes'

name 'inventory'
description 'Item and inventory management'
version '2.4.0'

shared_script 'config.lua'
client_scripts { 'client/main.lua', 'client/ui.lua' }
server_scripts { '@oxmysql/lib/MySQL.lua', 'server/main.lua' }
files { 'html/index.html', 'html/app.js' }
ui_page 'html/index.html'
dependencies { 'oxmysql', 'es_extended' }
exports { 'GetItem', 'AddItem' }
server_export 'RemoveItem'
