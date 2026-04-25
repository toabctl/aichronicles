<!--
  The homepage of the docs site mirrors the repository README so we
  keep one source of truth for the project pitch + architecture
  diagram. The include-markdown plugin handles the transclusion at
  build time; readers on github.io see the same content GitHub
  shows on the repo landing page.
-->

{%
  include-markdown "../README.md"
  rewrite-relative-urls=true
%}
