#include "no_such_file.h"

inherit "/std/thing";

static void
create()
{
    int x, n;
    string s, a, b;

    ::create();
    if (x = 1)
        n = 2;
    if ((x = 1))
        n = 3;
    for (x = 0; x = sizeof(s) && n; x++)
        s = "hi";
    x == 1;
    n = sscanf(s, "%d", a, b);
    n = sscanf(s, "%s %% %s", a, b);
    this_object()->takes_two(1);
    call_out("takes_two", 5, 1, 2, 3);
    call_out("takes_two", 5, 1, 2);
    this_object()->flex(1, 2, 3, 4);
    if (x = sizeof(s)) n = 5;
}
