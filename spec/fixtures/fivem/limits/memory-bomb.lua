-- Doubling concatenation: must fail with the allocation budget before the
-- process grows without bound. string.rep is bounded separately.
fx_version 'cerulean'
game 'gta5'
local s = 'xxxxxxxxxxxxxxxx'
while true do
  s = s .. s
end
