-- Must fail with the output cap: far more recorded entries than any real
-- manifest declares.
fx_version 'cerulean'
game 'gta5'
local t = {}
for i = 1, 20000 do
  t[i] = 'stream/file' .. i .. '.ytd'
end
files(t)
