project = "Pitaya"
copyright = "2018, TFGCo"
author = "TFGCo"

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
htmlhelp_basename = "Pitayadoc"

latex_documents = [
    (root_doc, "Pitaya.tex", "Pitaya Documentation", "TFGCo", "manual"),
]

man_pages = [
    (root_doc, "pitaya", "Pitaya Documentation", [author], 1),
]

texinfo_documents = [
    (
        root_doc,
        "Pitaya",
        "Pitaya Documentation",
        author,
        "Pitaya",
        "Pitaya game server framework documentation.",
        "Miscellaneous",
    ),
]
