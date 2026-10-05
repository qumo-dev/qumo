def edit(p,pairs):
    s=open(p,encoding='utf-8',newline='').read()
    for old,new in pairs:
        assert s.count(old)==1,(p,old[:70]); s=s.replace(old,new)
    open(p,'w',encoding='utf-8',newline='').write(s)
edit('internal/auth/cmd.go',[
(r'''//	                 (qumo auth keygen writes one), as a path or a file://
//	                 URL. QUMO_AUTH_KEYS_FILE, its earlier name, is still
//	                 read when it is unset.''',r'''//	                 (qumo auth keygen writes one), as a path or a file://
//	                 URL.'''),
(r'''	keys := os.Getenv("QUMO_AUTH_KEYS")
	if keys == "" {
		keys = os.Getenv("QUMO_AUTH_KEYS_FILE")
	}
	if keys == "" {''',r'''	keys := os.Getenv("QUMO_AUTH_KEYS")
	if keys == "" {'''),
])
edit('internal/auth/cmd_test.go',[
(r'''		keys        string
		keysFile    string // the earlier name
		want        serveConfig''',r'''		keys        string
		want        serveConfig'''),
(r'''		"earlier name":        {keysFile: "old.json", want: serveConfig{addr: defaultAddr, keysFile: "old.json"}},
		"new name wins":       {keys: "new.json", keysFile: "old.json", want: serveConfig{addr: defaultAddr, keysFile: "new.json"}},
''',''),
(r'''			t.Setenv("QUMO_AUTH_KEYS", tt.keys)
			t.Setenv("QUMO_AUTH_KEYS_FILE", tt.keysFile)''',r'''			t.Setenv("QUMO_AUTH_KEYS", tt.keys)'''),
])
print('ok')
