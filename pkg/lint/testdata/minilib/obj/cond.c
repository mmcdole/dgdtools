inherit "/std/thing";

static void tick();

static void
create()
{
    ::create();
    call_out("tick", 5);
    store_fp("/std/thing", "takes_two");    /* registration: fire-time args unknown */
}

static void
tick()
{
    int i;
#ifdef __DGD__
    if (sizeof(({ 1 })) &&
#else
    if (sizeof(({ 1, 2 })) &&
#endif
        i > 0) {
        i = 1;
    }
}
