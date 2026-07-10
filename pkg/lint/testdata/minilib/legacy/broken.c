#include <paths.h>

inherit STD_MISSING;        /* resolves to a nonexistent object */
inherit TOTALLY_UNKNOWN;    /* unresolved-inherit */

static void
create()
{
    ::create();
    this_object()->anything_at_all();   /* chain partial: must NOT be flagged */
}
