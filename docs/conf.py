project = "Alkaid Pitaya"
copyright = "2018, TFGCo; Alkaid Pitaya contributors"
author = "Alkaid Pitaya contributors"

version = ""
release = ""

needs_sphinx = "9.1"

extensions = [
    "myst_parser",
    "sphinxcontrib.mermaid",
]

myst_fence_as_directive = ["mermaid"]

source_suffix = {
    ".rst": "restructuredtext",
    ".md": "markdown",
}
root_doc = "index"
language = "en"
exclude_patterns = ["_build", ".venv", "Thumbs.db", ".DS_Store"]
pygments_style = "sphinx"

html_theme = "sphinx_rtd_theme"
htmlhelp_basename = "AlkaidPitayadoc"

latex_documents = [
    (root_doc, "Pitaya.tex", "Alkaid Pitaya Documentation", author, "manual"),
]

man_pages = [
    (root_doc, "pitaya", "Alkaid Pitaya Documentation", [author], 1),
]

texinfo_documents = [
    (
        root_doc,
        "Pitaya",
        "Alkaid Pitaya Documentation",
        author,
        "Pitaya",
        "Pitaya game server framework documentation.",
        "Miscellaneous",
    ),
]
