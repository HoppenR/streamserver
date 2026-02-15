local streamserver_root = os.getenv("STREAMSERVER_HOME")

if not streamserver_root then return end

return {
    name = 'gopls',
    cmd = { 'gopls', 'serve' },
    filetypes = { 'go', 'gomod', 'gowork', 'gotmpl' },
    root_dir = streamserver_root,
    settings = {
        gopls = {
            analyses = {
                unusedparams = true,
                shadow = true,
            },
            staticcheck = true,
            gofumpt = true,
            completeUnimported = true,
            usePlaceholders = true,
            directoryFilters = { "-.git", "-.vscode", "-.idea", "-.bin", "-bin" },
        },
    },
    init_options = {
        usePlaceholders = true,
    },
}
