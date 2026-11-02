package main

// rtkHook would chain RTK's own hook (a token-saving command rewrite). The plugin leaves
// that to RTK's own hook, if installed, so it answers like a missing rtk: no rewrite.
var rtkHook = func(stdin []byte) ([]byte, []byte, int) { return nil, nil, 0 }
