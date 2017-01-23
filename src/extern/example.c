#include <stdio.h>
#include "../../include/extern/accumulo.h"

int
main (int argc, char **argv[])
{
  printf ("Hello world");
  struct connector *connector = create_connector ("instance", "localhost:2181",
						  "root", "secret");
  free_connector (connector);
  return 0;
}
