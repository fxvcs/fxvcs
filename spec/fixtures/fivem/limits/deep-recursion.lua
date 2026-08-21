-- Must fail with the recursion (call-depth) limit.
fx_version 'cerulean'
game 'gta5'
local function down(n)
  return down(n + 1) + 1
end
client_script(tostring(down(0)))
