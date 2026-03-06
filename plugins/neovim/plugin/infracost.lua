vim.api.nvim_create_autocmd("FileType", {
  pattern = "terraform",
  callback = function()
    local root = vim.fs.dirname(vim.fs.find("infracost.yml", { upward = true })[1])
      or vim.fn.fnamemodify(vim.api.nvim_buf_get_name(0), ":p:h")
    vim.lsp.start({
      name = "infracost",
      cmd = { "infracost-ls" },
      root_dir = root,
      capabilities = vim.lsp.protocol.make_client_capabilities(),
    })
  end,
})
