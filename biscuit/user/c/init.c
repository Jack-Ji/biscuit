#include <litc.h>

int main(int argc, char **argv)
{
	printf("init starting...\n");

	// create dev nodes
	mkdir("/dev", 0);
	int ret;
	ret = mknod("/dev/console", 0, MKDEV(1, 0));
	if (ret != 0 && ret != -EEXIST)
		err(ret, "mknod");

	for (;;) {
		int pid = fork();
		if (!pid) {
			char * const args[] = {"/bin/lsh", NULL};
			ret = execv(args[0], args);
			err(ret, "execv");
		}
		wait(NULL);
		printf("lsh terminated?\n");
	}
	return 0;
}